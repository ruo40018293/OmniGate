package app

// workbuddy_models_json.go 一键把全部模型写入 WorkBuddy 的自定义模型清单
// （%USERPROFILE%\.workbuddy\models.json）。
//
// ## 场景
//
// 用户在 WorkBuddy 的「自定义模型」里要手工敲 `渠道/模型 (倍率)` 才能用上本工具的
// 全部渠道。模型一多（当前 100+ 个）手工维护不现实，故提供一键写入。
//
// ## 为什么必须先备份
//
// models.json 是**用户的资产**：里面有他手工加的条目、可能还有别的工具写入的内容，
// 可能有手工调过的 apiKey/url。我们是「整体覆盖」写入，直接盖掉等于静默丢数据。
//
// 故：写入前无条件备份为 models.json.bak-<时间戳>，同目录留档。
//
// ## 幂等与安全
//
//   - 生成的条目 id 带倍率后缀（与 /v1/models 一致），客户端直接可用；
//   - apiKey/url 取**当前生效配置**，不写死 —— 换端口或改 key 后重跑一次即可同步；
//   - tmp + rename 原子替换，避免 WorkBuddy 读到半截 JSON；
//   - 只在 Windows 之外的平台返回明确错误（WorkBuddy 是 Windows 应用，
//     但路径探测本身跨平台安全，故不硬拦）。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"omnigate/internal/provider"
)

// WorkBuddyModelEntry 与 WorkBuddy 自定义模型清单的单条结构严格对齐。
//
// 字段名/类型照抄用户现有 models.json 的实测形状（42 条实测样本统计一致）：
// 全部条目都带这 9 个键，vendor 恒为 "Custom"、useCustomProtocol 恒 false。
// 多写/少写字段都可能被客户端忽略，故保持完全一致。
type WorkBuddyModelEntry struct {
	APIKey            string `json:"apiKey"`
	ID                string `json:"id"`
	Name              string `json:"name"`
	SupportsImages    bool   `json:"supportsImages"`
	SupportsReasoning bool   `json:"supportsReasoning"`
	SupportsToolCall  bool   `json:"supportsToolCall"`
	URL               string `json:"url"`
	UseCustomProtocol bool   `json:"useCustomProtocol"`
	Vendor            string `json:"vendor"`
}

// ModelsJSONResult 一键写入结果（面板展示用）。
type ModelsJSONResult struct {
	Path    string `json:"path"`
	Backup  string `json:"backup,omitempty"` // 有覆盖才非空
	Count   int    `json:"count"`
	Skipped int    `json:"skipped"` // 无倍率、未纳入的模型数
	Note    string `json:"note,omitempty"`
	Existed bool   `json:"existed"` // 覆盖前文件是否已存在
}

// ModelsJSONPath 返回本机 WorkBuddy 自定义模型清单路径。
//
// %USERPROFILE% 由 os.UserHomeDir() 提供（Windows 下等价于 USERPROFILE）。
// 不读环境变量 USERPROFILE：Go 的 os.UserHomeDir 已在 Windows 上按优先级处理过。
func ModelsJSONPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("无法确定用户主目录：%w", err)
	}
	return filepath.Join(home, ".workbuddy", "models.json"), nil
}

// WriteWorkBuddyModelsJSON 把当前全部可用模型写入 WorkBuddy 自定义模型清单。
//
// 仅收录**上游明确给了倍率**的模型（priced）：无倍率的条目对用户没有参考价值，
// 反而会让清单被大量 "未知价格" 模型淹没 —— 那正是用户想解决的问题。
//
// exclude 为要跳过的渠道（如旧 Qoder 已下线）。
func (a *App) WriteWorkBuddyModelsJSON(exclude []provider.Kind) (*ModelsJSONResult, error) {
	path, err := ModelsJSONPath()
	if err != nil {
		return nil, err
	}

	// 端点与 key 取当前生效配置：写死后端口/密钥变更就同步不了。
	apiKey := a.adminAPIKey()
	endpoint := a.modelsJSONEndpoint()
	if apiKey == "" {
		return nil, fmt.Errorf("当前未设置 API Key，无法写入自定义模型（客户端会用它鉴权）")
	}

	skipStr := make(map[string]bool, len(exclude))
	for _, k := range exclude {
		skipStr[k.String()] = true
	}

	// 复用费率表那套「渠道 → 模型列表」，保证写入的模型集合与面板所见完全一致
	// （同一个 buildFeesChannels，避免两处逻辑漂移）。
	a.pricingMu.Lock()
	cached := a.pricingCache
	a.pricingMu.Unlock()
	if len(cached) == 0 {
		return nil, fmt.Errorf("尚未获取到任何倍率数据，请先点「刷新费率」再试")
	}
	var modelsByKind map[provider.Kind][]provider.ModelInfo
	if a.handler != nil {
		modelsByKind = a.handler.CachedChannelModels()
	}
	chans := buildFeesChannels(modelsByKind, cached, feeChannelOrder)
	entries := make([]WorkBuddyModelEntry, 0, 128)
	skipped := 0
	for _, ch := range chans {
		if skipStr[ch.Channel] {
			continue
		}
		for _, m := range ch.Models {
			if !m.Priced {
				skipped++
				continue
			}
			// id 带倍率后缀：与 /v1/models 形态一致，客户端显示与粘贴都用它。
			id := ch.Channel + "/" + m.Model + provider.RateSuffixFormat(m.Rate, true)
			entries = append(entries, WorkBuddyModelEntry{
				APIKey:            apiKey,
				ID:                id,
				Name:              id,
				SupportsImages:    m.SupportsImages,
				SupportsReasoning: m.SupportsReasoning,
				SupportsToolCall:  m.SupportsTools,
				URL:               endpoint,
				UseCustomProtocol: false,
				Vendor:            "Custom",
			})
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("没有可写入的模型：上游尚未返回任何倍率（先点「刷新费率」再试）")
	}
	// 稳定排序：同渠道内按倍率升序在前？—— 不，沿用 feesChannels 的排序即可，
	// 但为保证多次运行产出字节一致（便于 diff/复现），按 id 排一次。
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })

	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return nil, err
	}

	res := &ModelsJSONResult{Path: path, Count: len(entries), Skipped: skipped}

	// 备份：只覆盖已存在的文件，且失败即中止（宁可不写，也不丢用户原有数据）。
	if st, serr := os.Stat(path); serr == nil && !st.IsDir() {
		res.Existed = true
		bak := fmt.Sprintf("%s.bak-%s", path, time.Now().Format("20060102-150405"))
		if berr := copyFileForBackup(path, bak); berr != nil {
			return nil, fmt.Errorf("备份原文件失败（已中止写入，原文件未改动）：%w", berr)
		}
		res.Backup = bak
	}

	if err := writeFileAtomic(path, append(raw, '\n'), 0o644); err != nil {
		return nil, fmt.Errorf("写入失败：%w", err)
	}
	if res.Existed {
		res.Note = fmt.Sprintf("已覆盖原有 %d 条自定义模型，原文件备份为 %s",
			countJSONArray(path), filepath.Base(res.Backup))
	} else {
		res.Note = "已新建自定义模型清单"
	}
	return res, nil
}

// modelsJSONHost 生成清单里 url 使用的 host。
//
// 监听 0.0.0.0/:: 时必须用**回环地址**而不是局域网 IP：清单是给本机 WorkBuddy 用的，
// 写局域网 IP 会在 IP 变化（换 WiFi/插拔网线）后直接失效。
func (a *App) modelsJSONHost() string {
	a.mu.Lock()
	h := a.cfg.Listen.Host
	a.mu.Unlock()
	switch h {
	case "", "0.0.0.0", "::", "localhost":
		return "127.0.0.1"
	}
	return h
}

// modelsJSONEndpoint 客户端要填的完整端点（预览与写入共用，保证两处一致）。
func (a *App) modelsJSONEndpoint() string {
	return fmt.Sprintf("http://%s:%d/v1/chat/completions", a.modelsJSONHost(), a.adminPort())
}

// adminAPIKey / adminPort 读当前生效的 API Key 与端口（不打印值）。
func (a *App) adminAPIKey() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.APIKey
}

func (a *App) adminPort() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.cfg.Listen.Port
	if p <= 0 {
		return 7863
	}
	return p
}

// writeFileAtomic tmp + rename 原子替换（同 internal/auth 的做法）。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// copyFileForBackup 复制文件用于备份（不用 os.Rename —— 那会把原文件移走）。
func copyFileForBackup(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

// countJSONArray 数一个 JSON 数组文件里的条目数（用于提示文案）；失败返回 0。
func countJSONArray(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return 0
	}
	return len(arr)
}

// trimModelsExclude 归一化前端传来的渠道列表（去空白、去空串、去重）。
func trimModelsExclude(in []string) []provider.Kind {
	seen := map[provider.Kind]bool{}
	out := make([]provider.Kind, 0, len(in))
	for _, s := range in {
		k := provider.Kind(strings.ToLower(strings.TrimSpace(s)))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}
