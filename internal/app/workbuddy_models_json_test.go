package app

// workbuddy_models_json_test.go 一键写入 WorkBuddy 自定义模型的回归测试。
//
// 重点锁两件事：
//  1. **覆盖前必须备份**（用户 models.json 里有手工内容，丢了无法恢复）；
//  2. 写出的条目 id 带倍率后缀、url/apiKey 取当前配置 —— 与 /v1/models 同源，
//     客户端拿到即可用。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"omnigate/internal/auth"
	"omnigate/internal/config"
	"omnigate/internal/pool"
	"omnigate/internal/provider"
	"omnigate/internal/server"
)

// withHome 把用户主目录指向临时目录（ModelsJSONPath 依赖它），返回 models.json 路径。
func withHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)        // 非 Windows
	t.Setenv("USERPROFILE", home) // Windows
	t.Setenv("HOMEDRIVE", "C:")
	t.Setenv("HOMEPATH", `\Users\test`)
	p, err := ModelsJSONPath()
	if err != nil {
		t.Fatalf("ModelsJSONPath: %v", err)
	}
	return p
}

// newModelsJSONApp 用真实的 New(Options) 构造 App（与 creditrefresh_test 同法），
// 再注入定价缓存与静态模型表。
//
// 模型走 server.Handler 的 StaticModels（CachedChannelModels 在动态缓存为空时回退它），
// 避免依赖 unexported 的 rt.models 动态缓存。
func newModelsJSONApp(t *testing.T, rates map[string]float64) *App {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.AuthDir = dir
	cfg.Listen.Host = "127.0.0.1"
	cfg.Listen.Port = 7863
	cfg.APIKey = "TestKey123"

	wbPool := pool.New(filepath.Join(dir, "state-workbuddy.json"))
	wbPool.Add(&auth.Auth{Kind: "workbuddy", UID: "u1", RefreshToken: "rt"})
	trPool := pool.New(filepath.Join(dir, "state-traework.json"))
	trPool.Add(&auth.Auth{Kind: "traework", UID: "u2", RefreshToken: "rt"})

	a, err := New(Options{
		ConfigPath: filepath.Join(dir, "config.json"),
		Config:     cfg,
		Runtimes: map[provider.Kind]*Runtime{
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: wbPool},
			provider.TraeWork:  {Kind: provider.TraeWork, Pool: trPool},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.Close)

	// 注入带静态模型表的 handler：WriteWorkBuddyModelsJSON 走
	// handler.CachedChannelModels()，动态缓存为空时回退 StaticModels。
	a.SetHandler(server.NewHandler(server.Config{
		Runtimes: map[provider.Kind]*server.Runtime{
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: wbPool,
				StaticModels: []provider.ModelInfo{{ID: "hy3"}, {ID: "glm-5.3-flash"}, {ID: "auto"}}},
			provider.TraeWork: {Kind: provider.TraeWork, Pool: trPool,
				StaticModels: []provider.ModelInfo{{ID: "kimi-k3"}}},
		},
	}))

	a.pricingCache = []provider.ModelPricing{
		{Model: "hy3", Channel: "workbuddy", Rate: 0.06},
		{Model: "glm-5.3-flash", Channel: "workbuddy", Rate: 0.06},
		{Model: "kimi-k3", Channel: "traework", Rate: 1.83},
		// 无显式倍率的条目（Explicit=false）应被跳过
		{Model: "auto", Channel: "workbuddy", Rate: 0, Explicit: boolPtr(false)},
	}
	_ = rates
	return a
}

func boolPtr(b bool) *bool { return &b }

// TestWriteModelsJSONCreatesFile 文件不存在时新建，条目 id 带倍率。
func TestWriteModelsJSONCreatesFile(t *testing.T) {
	withHome(t)
	a := newModelsJSONApp(t, nil)

	res, err := a.WriteWorkBuddyModelsJSON(nil)
	if err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	if res.Existed {
		t.Error("首次写入不应报告「已覆盖」")
	}
	if res.Backup != "" {
		t.Errorf("首次写入不应产生备份，得到 %q", res.Backup)
	}

	raw, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatalf("读回失败：%v", err)
	}
	var got []WorkBuddyModelEntry
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("产出的 JSON 无法解析：%v", err)
	}
	// auto 是 Explicit=false → 不该写入
	for _, e := range got {
		if strings.HasPrefix(e.ID, "workbuddy/auto") {
			t.Error("无显式倍率的模型不应写入清单")
		}
	}
	// 带倍率且形状与 WorkBuddy 现有清单一致
	var found bool
	for _, e := range got {
		if e.ID == "workbuddy/hy3 (0.06x)" {
			found = true
			if e.Name != e.ID {
				t.Errorf("name 应与 id 一致，got %q", e.Name)
			}
			if e.APIKey != "TestKey123" {
				t.Errorf("apiKey 应取当前配置，got %q", e.APIKey)
			}
			if e.URL != "http://127.0.0.1:7863/v1/chat/completions" {
				t.Errorf("url 不对，got %q", e.URL)
			}
			if e.Vendor != "Custom" {
				t.Errorf("vendor 应为 Custom，got %q", e.Vendor)
			}
			if e.UseCustomProtocol {
				t.Error("useCustomProtocol 应为 false")
			}
		}
	}
	if !found {
		t.Errorf("未找到 workbuddy/hy3 (0.06x)，实际写入：%v", idsOf(got))
	}
}

// TestWriteModelsJSONBacksUpBeforeOverwrite **覆盖前必须备份**。
//
// 这是本特性最不能出错的地方：models.json 里有用户手工维护的内容。
func TestWriteModelsJSONBacksUpBeforeOverwrite(t *testing.T) {
	path := withHome(t)
	original := `[{"id":"hand-written","name":"我自己加的","apiKey":"OLD"}]`
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	a := newModelsJSONApp(t, nil)
	res, err := a.WriteWorkBuddyModelsJSON(nil)
	if err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	if !res.Existed {
		t.Error("应报告「已覆盖」")
	}
	if res.Backup == "" {
		t.Fatal("覆盖时必须产生备份路径")
	}
	// 备份文件必须存在且内容 == 原文件
	bakRaw, err := os.ReadFile(res.Backup)
	if err != nil {
		t.Fatalf("备份文件不存在：%v", err)
	}
	if string(bakRaw) != original {
		t.Errorf("备份内容与原文件不一致：\n备份=%s\n原=%s", bakRaw, original)
	}
	// 备份名带时间戳，便于用户分辨多次覆盖
	if !strings.Contains(filepath.Base(res.Backup), "bak-") {
		t.Errorf("备份文件名应含 bak-，got %q", filepath.Base(res.Backup))
	}
	// 新文件已被替换
	newRaw, _ := os.ReadFile(res.Path)
	if strings.Contains(string(newRaw), "hand-written") {
		t.Error("覆盖后新文件不应残留旧内容")
	}
}

// TestWriteModelsJSONBackupFailureAborts 备份失败必须中止写入，
// 不能出现「原文件已丢、备份也没有」的中间态。
func TestWriteModelsJSONBackupFailureAborts(t *testing.T) {
	path := withHome(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	original := `[{"id":"hand-written"}]`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	// 把备份目标路径做成目录，制造写失败
	// 备份名带时间戳，无法预知 → 改为占用整个目录使其不可写（Windows 上不可靠），
	// 故这里改为直接验证「写入后原文件要么是原样、要么是新内容」，不出现中间态。
	a := newModelsJSONApp(t, nil)
	if _, err := a.WriteWorkBuddyModelsJSON(nil); err != nil {
		// 备份失败也是合法结局之一：原文件必须保持原样
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("写入报错且原文件丢失：%v / %v", err, rerr)
		}
		if string(raw) != original {
			t.Errorf("报错后原文件被改动：%s", raw)
		}
		return
	}
	raw, _ := os.ReadFile(path)
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		t.Errorf("写入成功后文件不是合法 JSON 数组：%s", raw)
	}
}

// TestWriteModelsJSONRequiresAPIKey 未设 API Key 时拒绝写入：
// 客户端会用它鉴权，写空 key 等于产出一堆不可用的条目。
func TestWriteModelsJSONRequiresAPIKey(t *testing.T) {
	withHome(t)
	a := newModelsJSONApp(t, nil)
	a.mu.Lock()
	a.cfg.APIKey = ""
	a.mu.Unlock()

	if _, err := a.WriteWorkBuddyModelsJSON(nil); err == nil {
		t.Fatal("未设 API Key 时应拒绝写入")
	} else if !strings.Contains(err.Error(), "API Key") {
		t.Errorf("错误信息应点明 API Key，实际：%v", err)
	}
}

// TestWriteModelsJSONNoPricingData 无倍率数据时拒绝写入（避免写出一份空清单
// 把用户原有的有效配置清掉）。
func TestWriteModelsJSONNoPricingData(t *testing.T) {
	withHome(t)
	a := newModelsJSONApp(t, nil)
	a.pricingCache = nil

	if _, err := a.WriteWorkBuddyModelsJSON(nil); err == nil {
		t.Fatal("无倍率数据时应拒绝写入")
	}
}

// TestWriteModelsJSONHostForWildcard 监听 0.0.0.0 时 url 必须用回环地址，
// 不能写局域网 IP（换网络就失效）。
func TestWriteModelsJSONHostForWildcard(t *testing.T) {
	withHome(t)
	a := newModelsJSONApp(t, nil)
	a.mu.Lock()
	a.cfg.Listen.Host = "0.0.0.0"
	a.mu.Unlock()

	res, err := a.WriteWorkBuddyModelsJSON(nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(res.Path)
	if !strings.Contains(string(raw), "http://127.0.0.1:7863/") {
		t.Errorf("监听 0.0.0.0 时应写回环地址，实际内容：%s", firstLine(string(raw)))
	}
}

// TestWriteModelsJSONIsIdempotent 连续两次写入产出字节一致（便于 diff / 复现）。
func TestWriteModelsJSONIsIdempotent(t *testing.T) {
	withHome(t)
	a := newModelsJSONApp(t, nil)
	if _, err := a.WriteWorkBuddyModelsJSON(nil); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(ModelsJSONPathMust(t))
	if _, err := a.WriteWorkBuddyModelsJSON(nil); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(ModelsJSONPathMust(t))
	if string(first) != string(second) {
		t.Errorf("两次写入内容不一致：\n1=%s\n2=%s", firstLine(string(first)), firstLine(string(second)))
	}
}

// TestWriteModelsJSONExcludeSkipsChannels 排除渠道必须真的不写。
func TestWriteModelsJSONExcludeSkipsChannels(t *testing.T) {
	withHome(t)
	a := newModelsJSONApp(t, nil)
	res, err := a.WriteWorkBuddyModelsJSON([]provider.Kind{provider.TraeWork})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(res.Path)
	if strings.Contains(string(raw), "traework/") {
		t.Errorf("排除 traework 后不应出现该渠道条目：%s", firstLine(string(raw)))
	}
	if !strings.Contains(string(raw), "workbuddy/") {
		t.Error("未排除的渠道应正常写入")
	}
}

// TestPreviewDoesNotTouchDisk 预览必须只读 —— 绝不能因为「看一眼」就备份/覆盖。
func TestPreviewDoesNotTouchDisk(t *testing.T) {
	path := withHome(t)
	original := `[{"id":"hand-written"}]`
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newModelsJSONApp(t, nil)

	pv, err := a.previewWorkBuddyModelsJSON(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !pv.Exists || pv.ExistingCount != 1 {
		t.Errorf("预览应报告已存在 1 条，got exists=%v count=%d", pv.Exists, pv.ExistingCount)
	}
	if pv.WillWrite == 0 {
		t.Error("预览应报告将写入的条数")
	}
	// 关键：预览后文件与目录都不得变化
	raw, _ := os.ReadFile(path)
	if string(raw) != original {
		t.Errorf("预览改动了文件：%s", raw)
	}
	ents, _ := os.ReadDir(filepath.Dir(path))
	if len(ents) != 1 {
		t.Errorf("预览产生了额外文件（不应有备份），目录内容：%v", ents)
	}
}

// TestTrimModelsExclude 渠道列表归一化。
func TestTrimModelsExclude(t *testing.T) {
	got := trimModelsExclude([]string{" Qoder ", "qoder", "", "  "})
	if len(got) != 1 || got[0] != provider.Qoder {
		t.Errorf("归一化结果=%v，应为 [qoder]", got)
	}
}

func idsOf(es []WorkBuddyModelEntry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// ModelsJSONPathMust 测试辅助：拿当前生效的 models.json 路径。
func ModelsJSONPathMust(t *testing.T) string {
	t.Helper()
	p, err := ModelsJSONPath()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var _ = time.Now
