// Package capture 出站请求的异常取证（R54）。
//
// 背景：上游的灰度内容校验和账号级风控（workbuddy CN 的 11-128、workbuddyai 的
// 11140 等）不能靠本地探针复现——判据在服务端，而服务端不告诉你它依据什么。
// 唯一可靠的定案手段是：**复现时那一刻的真实出站字节 + 上游真实回包**。
//
// R49 曾加过 OMNIGATE_CAPTURE_CHAT 环境变量钩子，但它有两个致命的使用缺陷：
//
//  1. 只在显式设 env 时捕获 —— 现实里没人会常设，等出了事去设，事件已经过去；
//  2. 全量捕获正常请求 —— 一个会话就能写出几十上百个几百 KB 的文件，磁盘和
//     分析成本都不可接受，所以事实上也没法常开。
//
// 本包把捕获改为**异常触发**：只在请求失败时落盘，正常请求零写入、零开销。
// 这样它可以永久常开，不需要任何人记得去开开关。
//
// 落盘内容（一次异常 = 一个文件）：
//   - 请求方法、URL（不含 token；token 只在头里，本包不写头）
//   - 渠道名、账号 uid、模型（辅助判断是哪个渠道哪个号）
//   - **出站请求体原文**（净化后的最终字节 —— 这正是要做二分的东西）
//   - **上游回包原文**（截断到 maxBodyBytes，防超大 HTML 错误页爆盘）
//   - 上游状态码、分类结果、时间戳
//
// 隐私边界：不写任何请求头，因此 Authorization / Cookie / 各类 token 一律不落盘。
// 请求体里可能含有用户对话内容，所以目录说明里明确写了"含对话内容，勿外传"。
package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// EnvDir 显式指定捕获目录（沿用 R49 的变量名，保持向后兼容）。
// 未设置时回退到 <数据目录>/data/chat-capture/。
const EnvDir = "OMNIGATE_CAPTURE_CHAT"

// EnvOff 置 1 时彻底关闭捕获（默认常开，用于用户不想留任何本地副本的场景）。
const EnvOff = "OMNIGATE_CAPTURE_OFF"

// maxBodyBytes 回包落盘上限。上游错误体一般是几百字节到几 KB，
// 但网关类错误页（openresty 502 之类）可能是整页 HTML，限一个上界防爆盘。
const maxBodyBytes = 64 << 10

// 目录上限。超过任一条就按修改时间从最旧删，保证长期常开不会无限增长。
//
// 为什么不能只封文件数：单个取证文件可达 ~900KB（长上下文的出站请求体 +
// 64KB 回包），500 个能堆到 400MB 以上；实测 115 个文件就已经 18MB。
// 按字节封顶才是真正起作用的那一条。
var (
	maxFiles              = 500            // 文件数上限
	maxBytes        int64 = 32 << 20       // 总字节上限（32MB）
	pruneSlackFiles       = 50             // 一次多删 50 个（滞后修剪，避免每个请求都扫目录）
	pruneSlackBytes       = int64(8 << 20) // 一次多删 8MB，同上
)

// dirOnce 缓存解析后的目录，避免每个请求都做一次 MkdirAll 系统调用。
var (
	dirMu   sync.Mutex
	dirPath string
	dirOK   bool
)

// enabled 报告是否应当捕获。
//
// 设计取舍：默认开启。因为本包只在**异常**时写盘，正常请求没有开销，
// 常开的收益（出事时一定有证据）远大于代价（几百 KB 磁盘）。
// 需要完全静默的用户用 OMNIGATE_CAPTURE_OFF=1 关掉。
func enabled() bool {
	if v := os.Getenv(EnvOff); v == "1" || strings.EqualFold(v, "true") {
		return false
	}
	return true
}

// Dir 返回捕获目录（必要时创建）。失败时返回空串，调用方应静默跳过。
func Dir() string {
	dirMu.Lock()
	defer dirMu.Unlock()
	if dirPath != "" || dirOK {
		return dirPath
	}
	d := os.Getenv(EnvDir)
	if d == "" {
		d = filepath.Join(baseDir(), "data", "chat-capture")
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		// 记一次失败就不再重试，避免每个请求都撞同一个不可写路径。
		dirPath, dirOK = "", true
		return ""
	}
	dirPath = d
	return dirPath
}

// baseDir 数据目录根，与 cmd/omnigate 的 workDir() 同一套规则：
// exe 所在目录（Windows 便携包即 exe 同级），不引入新的目录约定。
func baseDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// ResetDirCache 清空目录缓存（测试用：t.Setenv 后需要重新解析）。
func ResetDirCache() {
	dirMu.Lock()
	dirPath, dirOK = "", false
	dirMu.Unlock()
}

// Record 一次异常请求的取证材料。
type Record struct {
	Channel  string // 渠道名，如 workbuddy / workbuddyai / qodercn
	UID      string // 账号 uid（不含 token）
	Model    string // 请求里的模型名（尽力而为，解析失败留空）
	Status   int    // 上游 HTTP 状态码；传输层失败时为 0
	Classify string // 渠道的 Classify() 结果，如 content_blocked / rate_limited
	Request  []byte // 出站请求体原文（净化后的最终字节）
	Response []byte // 上游回包原文
	Err      string // 传输层错误文本（status=0 时）
}

// Save 把一个异常记录落盘。**永远不会 panic，也不会返回错误影响主流程**——
// 取证是辅助能力，绝不能因为它失败而让真正的请求失败。
//
// 返回落盘路径（空串表示未落盘，调用方无需关心原因）。
func Save(rec Record) string {
	if !enabled() {
		return ""
	}
	dir := Dir()
	if dir == "" {
		return ""
	}

	// 目录文件数上限：超了删最旧的，避免长跑几个月后堆几万个文件。
	pruneIfNeeded(dir)

	resp := rec.Response
	truncated := false
	if len(resp) > maxBodyBytes {
		resp = resp[:maxBodyBytes]
		truncated = true
	}

	now := time.Now()
	name := fmt.Sprintf("err-%s-%s-%d.json",
		now.Format("20060102-150405.000"), sanitizeName(rec.Channel), rec.Status)
	if len(rec.Request) > 0 {
		name = fmt.Sprintf("err-%s-%s-%d-%d.json",
			now.Format("20060102-150405.000"), sanitizeName(rec.Channel), rec.Status, len(rec.Request))
	}

	payload := map[string]any{
		"ts":                 now.Format(time.RFC3339Nano),
		"channel":            rec.Channel,
		"uid":                rec.UID,
		"model":              rec.Model,
		"status":             rec.Status,
		"classify":           rec.Classify,
		"transport_error":    rec.Err,
		"request_bytes":      len(rec.Request),
		"response_bytes":     len(rec.Response),
		"response_truncated": truncated,
		// 请求体按原样存字符串（不是 base64），因为要能直接用 grep/编辑器二分。
		"request":  string(rec.Request),
		"response": string(resp),
		"note":     "本文件含对话内容，仅用于本地排障，勿外传",
	}
	buf, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return ""
	}
	full := filepath.Join(dir, name)
	if err := os.WriteFile(full, buf, 0o644); err != nil {
		return ""
	}
	// 写完再看一次：写前修剪留的余量按「滞后目标」算，新文件可能刚好把它顶回
	// 上限之上（单文件可达 ~900KB）。取证是异常路径，多扫一次目录无所谓；
	// 「目录不会超过 maxBytes」这条承诺必须落盘后也成立。
	pruneIfNeeded(dir)
	atomic.AddInt64(&savedCount, 1)
	return full
}

// SaveSystem 落一份「系统级」（渠道无关）证据：上游只发保活碎片、零内容断开，
// 或客户端中途取消 —— 这类失败在渠道层看不出是「排队」还是「网络」，必须把
// **当时那一刻的真实连接行为**留下来，事后才可能是可分析的。
//
// 与 Save 的区别：Save 是渠道异常（带模型/状态/上游原文）；这里是网关侧观测到的
// 连接现象（读了多久、读到什么碎片、客户端是否还在），用于判定「沉默 180s」
// 到底是上游挂起、还是网关自己卡住。
//
// 同样**永不 panic、永不返回错误影响主流程**。
func SaveSystem(name string, meta map[string]any) string {
	if !enabled() || name == "" {
		return ""
	}
	dir := Dir()
	if dir == "" {
		return ""
	}
	pruneIfNeeded(dir)
	now := time.Now()
	full := filepath.Join(dir, fmt.Sprintf("sys-%s-%s.json",
		now.Format("20060102-150405.000"), sanitizeName(name)))
	payload := map[string]any{
		"ts":   now.Format(time.RFC3339Nano),
		"kind": "system",
		"note": "系统级取证：非对话内容，用于排查网关侧行为",
	}
	for k, v := range meta {
		payload[k] = v
	}
	buf, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return ""
	}
	if err := os.WriteFile(full, buf, 0o644); err != nil {
		return ""
	}
	pruneIfNeeded(dir) // 同 Save：落盘后补一次，保证总字节上限成立
	atomic.AddInt64(&savedCount, 1)
	return full
}

var savedCount int64

// SavedCount 已落盘的异常记录数（面板/测试用）。
func SavedCount() int64 { return atomic.LoadInt64(&savedCount) }

// sanitizeName 把渠道名收成安全的文件名片段。
func sanitizeName(s string) string {
	if s == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// pruneIfNeeded 修剪取证目录：文件数超过 maxFiles 或总字节超过 maxBytes 时，
// 按修改时间从最旧的开始删，直到回到「滞后目标」（上限减掉一段 slack）以内。
//
// 只按文件数封顶是不够的：单文件可达 ~900KB，500 个能堆到 400MB+。字节数才是
// 真正决定磁盘占用的那一条，所以两条都查、两条都修。
//
// 只在真正要写盘时调用；读目录失败就放弃修剪（绝不阻塞写盘）。
func pruneIfNeeded(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type entry struct {
		name string
		mod  time.Time
		size int64
	}
	items := make([]entry, 0, len(ents))
	var total int64
	for _, e := range ents {
		// 只修剪自己写的 .json；目录里别的东西（手工备注、外部拷贝）一律不碰。
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, entry{e.Name(), info.ModTime(), info.Size()})
		total += info.Size()
	}
	// 快路径：两个上限都没超，直接返回（绝大多数请求走这里）。
	if len(items) < maxFiles && total <= maxBytes {
		return
	}

	wantCount := maxFiles - pruneSlackFiles
	if wantCount < 0 {
		wantCount = 0
	}
	wantBytes := maxBytes - pruneSlackBytes
	if wantBytes < 0 {
		wantBytes = 0
	}

	sort.Slice(items, func(i, j int) bool { return items[i].mod.Before(items[j].mod) })
	left, bytes := len(items), total
	for _, it := range items {
		if left <= wantCount && bytes <= wantBytes {
			return
		}
		if err := os.Remove(filepath.Join(dir, it.name)); err != nil {
			// 删不掉（被占用/权限）就不要把计数减掉：下轮写盘时还会再试。
			continue
		}
		left--
		bytes -= it.size
	}
}

// ModelFromBody 尽量从请求体里取模型名，仅用于取证标注。
// 解析失败返回空串，绝不报错。
func ModelFromBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return probe.Model
}

// Str 小工具：把状态码转成文件名片段（保留以兼容调用方的整数拼接习惯）。
func Str(n int) string { return strconv.Itoa(n) }
