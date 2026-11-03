package raccoon

import (
	"net/http"
	"strings"
	"testing"

	"omnigate/internal/auth"
)

// TestApplyHeadersMatchesOfficialLLMPath 逐项锁死官方 box-agent LLM 路径的头形态。
//
// 取证见 headers.go 文件头：UA 必须是 openai SDK 的 AsyncOpenAI/Python 2.8.0
// （不是 Electron 形态），X-Stainless-* 六件必须齐，x-client-* 五件必须齐
// （device_id 官方恒空 ⇒ 本渠道不发）。
//
// 变异点：删掉 User-Agent 那一行（回到裸奔的 Go-http-client），本测试必须失败。
func TestApplyHeadersMatchesOfficialLLMPath(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://api.example/chat", nil)
	ApplyHeaders(req, "tok", "", "10.0.26100", "sess-0-abcdef12", "11111111-2222-4333-8444-555555555555")

	if got := req.Header.Get("User-Agent"); got != OfficialUA {
		t.Errorf("User-Agent=%q，官方是 %q —— 缺它会退化成 Go-http-client/1.1，是裸奔级指纹（R91）",
			got, OfficialUA)
	}
	if got := req.Header.Get("Accept"); got != "" {
		t.Errorf("不应手写 Accept=%q：官方 SDK 会自己发 application/json（本渠道曾误加）", got)
	}

	// SDK platform_headers 六件（openai/_base_client.py）。
	for _, k := range []string{
		"X-Stainless-Lang", "X-Stainless-Package-Version", "X-Stainless-OS",
		"X-Stainless-Arch", "X-Stainless-Runtime", "X-Stainless-Runtime-Version",
	} {
		if v := req.Header.Get(k); v == "" {
			t.Errorf("%s 缺失：官方 SDK 每个请求都发（R91）", k)
		}
	}
	if got := req.Header.Get("X-Stainless-Lang"); got != "python" {
		t.Errorf("X-Stainless-Lang=%q want python", got)
	}
	if got := req.Header.Get("X-Stainless-Package-Version"); got != sdkVersion {
		t.Errorf("X-Stainless-Package-Version=%q want %q", got, sdkVersion)
	}
	if got := req.Header.Get("X-Stainless-Runtime"); got != "CPython" {
		t.Errorf("X-Stainless-Runtime=%q want CPython", got)
	}
	if got := req.Header.Get("X-Stainless-Runtime-Version"); got != pyVersion {
		t.Errorf("X-Stainless-Runtime-Version=%q want %q", got, pyVersion)
	}

	// x-client-* 五件（ClientInfo.headers_for_url）。
	for _, k := range []string{
		"X-Client-Name", "X-Client-Platform", "X-Client-Version",
		"X-Client-OS-Version", "X-Client-Channel",
	} {
		if v := req.Header.Get(k); v == "" {
			t.Errorf("%s 缺失：官方 headers_for_url 逐项 cleanHeaderValue 后非空才发（R91）", k)
		}
	}
	if got := req.Header.Get("X-Client-Name"); got != clientName {
		t.Errorf("X-Client-Name=%q want %q", got, clientName)
	}
	if got := req.Header.Get("X-Client-Version"); got != clientVersion {
		t.Errorf("X-Client-Version=%q want %q（官方带 v 前缀）", got, clientVersion)
	}
	if got := req.Header.Get("X-Client-Channel"); got != clientChannel {
		t.Errorf("X-Client-Channel=%q want %q", got, clientChannel)
	}
	if got := req.Header.Get("X-Client-OS-Version"); got != "10.0.26100" {
		t.Errorf("X-Client-OS-Version=%q want 10.0.26100（应透传入参，不是常量）", got)
	}

	// 官方 device_id 恒空 ⇒ 本渠道一个头都不许自造。
	for _, k := range []string{"X-Client-Device-Id", "X-Client-Device-ID", "X-Raccoon-Language"} {
		if v := req.Header.Get(k); v != "" {
			t.Errorf("%s=%q 必须不发：官方该值为空/该头属渲染层（R91）", k, v)
		}
	}
	// 不伪造 Title / Call-Kind：网关无对应语义，硬填会把主对话误标成内部调用。
	for _, k := range []string{"X-Raccoon-Title", "X-Racoon-Call-Kind", "X-Raccoon-Call-Kind"} {
		if v := req.Header.Get(k); v != "" {
			t.Errorf("%s=%q 不得伪造（官方由宿主填，网关无此语义）", k, v)
		}
	}

	if got := req.Header.Get("X-RACCOON-Session-ID"); got != "sess-0-abcdef12" {
		t.Errorf("X-RACCOON-Session-ID=%q want 透传入参", got)
	}
	if got := req.Header.Get("X-RACCOON-Turn-ID"); got != "11111111-2222-4333-8444-555555555555" {
		t.Errorf("X-RACCOON-Turn-ID=%q want 透传入参", got)
	}
	// Content-Type/Authorization 仍要设（原有契约）。
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type=%q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("Authorization=%q", got)
	}
}

// TestApplyHeadersSkipsEmptyValues 锁「空值不发」：官方 _clean_header_value 规则。
func TestApplyHeadersSkipsEmptyValues(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://api.example/chat", nil)
	ApplyHeaders(req, "", "", "", "", "")
	if v := req.Header.Get("Authorization"); v != "" {
		t.Errorf("空 token 不该发 Authorization=%q", v)
	}
	if v := req.Header.Get("X-Org-Code"); v != "" {
		t.Errorf("个人账号（空 orgCode）不该发 X-Org-Code=%q", v)
	}
	if v := req.Header.Get("X-Client-OS-Version"); v != "" {
		t.Errorf("空 osVersion 不该发 X-Client-OS-Version=%q（官方空值跳过该头）", v)
	}
	if v := req.Header.Get("X-RACCOON-Session-ID"); v != "" {
		t.Errorf("空 sessID 不该发 =%q", v)
	}
	if v := req.Header.Get("X-RACCOON-Turn-ID"); v != "" {
		t.Errorf("空 turnID 不该发 =%q", v)
	}
	// 但 UA / X-Stainless-* / x-client-* 的常量项仍在。
	if got := req.Header.Get("User-Agent"); got != OfficialUA {
		t.Errorf("User-Agent=%q 与 token 无关，必须照发", got)
	}
}

// TestCleanHeaderValue 对齐官方 _clean_header_value：strip → 必须全 ASCII → ≤256。
func TestCleanHeaderValue(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  v1.0.38  ", "v1.0.38"},
		{"", ""},
		{"   ", ""},
		{"浣熊", ""},  // 非 ASCII 一律丢弃
		{"ok浣", ""}, // 混了非 ASCII 也整条丢弃
		{strings.Repeat("a", 256), strings.Repeat("a", 256)},
		{strings.Repeat("a", 257), ""}, // 超长丢弃
		{"desktop-windows-x86", "desktop-windows-x86"},
	}
	for _, c := range cases {
		if got := cleanHeaderValue(c.in); got != c.want {
			t.Errorf("cleanHeaderValue(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// TestClientPlatformOfShape 锁 normalizeDesktopPlatform 的输出形态：
// `desktop-<macos|windows|linux|unknown>-<x86|arm|arm64|unknown>`。
//
// 注意本测试只锁形态（可观测的取值集合是硬证据），具体 GOARCH 分支映射是推断。
func TestClientPlatformOfShape(t *testing.T) {
	got := clientPlatformOf()
	if !strings.HasPrefix(got, "desktop-") {
		t.Fatalf("clientPlatformOf()=%q 必须以 desktop- 开头", got)
	}
	parts := strings.Split(got, "-")
	if len(parts) != 3 {
		t.Fatalf("clientPlatformOf()=%q 必须三段（desktop-<os>-<arch>）", got)
	}
	switch parts[1] {
	case "macos", "windows", "linux", "unknown":
	default:
		t.Errorf("clientPlatformOf() 的 os 段=%q 不在官方取值集内", parts[1])
	}
	switch parts[2] {
	case "x86", "arm", "arm64", "unknown":
	default:
		t.Errorf("clientPlatformOf() 的 arch 段=%q 不在官方取值集内（注意官方无 x64）", parts[2])
	}
}

// TestStainlessArchFollowsSDK 锁 SDK get_architecture 的判定顺序。
//
// 关键：Windows 上 platform.machine()=="AMD64" 落进 OtherArch，官方实际发
// `other:amd64`，不是 x64。照抄真实客户端比"纠正"它更安全。
func TestStainlessArchFollowsSDK(t *testing.T) {
	got := stainlessArch()
	if got == "" {
		t.Fatalf("stainlessArch() 不得为空")
	}
	switch got {
	case "arm64", "arm", "x64", "x32":
	default:
		if !strings.HasPrefix(got, "other:") {
			t.Errorf("stainlessArch()=%q 既不属 SDK 具名取值，也不是 other:<machine> 形态", got)
		}
	}
}

// TestSessionReusedWithinRotation 锁 R91 的核心行为：同 key 在 24h 内恒定复用。
//
// 变异点：把 sessionForEx 改成每次新建（去掉查表分支），本测试必须失败。
func TestSessionReusedWithinRotation(t *testing.T) {
	resetSessions()
	key := "acct|model"
	first, new1 := sessionForEx(key)
	if !new1 {
		t.Fatalf("首次访问必须 new=true")
	}
	if !strings.HasPrefix(first, "sess-0-") || len(first) != len("sess-0-")+8 {
		t.Fatalf("会话 ID 形态 %q 不符官方 sess-<N>-<8hex>", first)
	}
	for i := 0; i < 5; i++ {
		again, isNew := sessionForEx(key)
		if isNew {
			t.Fatalf("第 %d 次复用却报 new=true —— 每请求新建会话正是 429 的触发维度", i+2)
		}
		if again != first {
			t.Fatalf("同一 key 的会话 ID 变了：%q -> %q", first, again)
		}
	}
}

// TestSessionIsolationByAccount 锁「会话不跨账号漂移」。
//
// 变异点：sessionKeyOf 去掉 accountKey（只按模型分桶），本测试必须失败。
func TestSessionIsolationByAccount(t *testing.T) {
	resetSessions()
	a := &auth.Auth{UID: "uA"}
	b := &auth.Auth{UID: "uB"}
	sidA := sessionFor(sessionKeyOf(a, "same-model"))
	sidB := sessionFor(sessionKeyOf(b, "same-model"))
	if sidA == sidB {
		t.Fatalf("两个账号同一模型共用同一会话 ID %q：上游会看到同一会话出现在两个账号", sidA)
	}
	// 不同模型同账号也必须分开（会话按「账号+模型」分桶）。
	sidA2 := sessionFor(sessionKeyOf(a, "other-model"))
	if sidA2 == sidA {
		t.Fatalf("同一账号两个模型共用同一会话 ID %q", sidA)
	}
}

// TestSessionRotationEventuallyRenews 锁 24h 轮换：过期后必须换新 ID 且序号递增。
func TestSessionRotationEventuallyRenews(t *testing.T) {
	resetSessions()
	key := "acct|model"
	first, _ := sessionForEx(key)
	// 直接把该 key 的到期时间拨到过去，模拟过了轮换期。
	forceExpire(key)
	second, isNew := sessionForEx(key)
	if !isNew {
		t.Fatalf("过期后必须 new=true")
	}
	if second == first {
		t.Fatalf("过期后会话 ID 未变：%q", second)
	}
	if !strings.HasPrefix(second, "sess-1-") {
		t.Fatalf("新会话序号应递增到 sess-1-，得到 %q", second)
	}
}

// TestAccountKeyFallbacks 锁账号标识的兜底链：UID → Nickname → token 指纹 → anon。
func TestAccountKeyFallbacks(t *testing.T) {
	if got := accountKey(nil); got != "anon" {
		t.Errorf("nil auth 应为 anon，得到 %q", got)
	}
	if got := accountKey(&auth.Auth{UID: "u1"}); got != "u1" {
		t.Errorf("优先 UID，得到 %q", got)
	}
	if got := accountKey(&auth.Auth{Nickname: "nick"}); got != "nick" {
		t.Errorf("无 UID 应退昵称，得到 %q", got)
	}
	tokA := accountKey(&auth.Auth{AccessToken: "ta"})
	tokB := accountKey(&auth.Auth{AccessToken: "tb"})
	if !strings.HasPrefix(tokA, "tok-") {
		t.Errorf("无 UID/Nickname 应退 token 指纹，得到 %q", tokA)
	}
	if tokA == tokB {
		t.Errorf("不同 token 的指纹必须不同：%q vs %q", tokA, tokB)
	}
	if got := accountKey(&auth.Auth{}); got != "anon" {
		t.Errorf("全空应退 anon，得到 %q", got)
	}
}

// TestTurnIDIsUUIDv4 锁 turn id 形态（官方每轮一个 uuid4）。
func TestTurnIDIsUUIDv4(t *testing.T) {
	for i := 0; i < 20; i++ {
		id := newTurnID()
		if len(id) != 36 {
			t.Fatalf("turn id %q 长度 %d != 36", id, len(id))
		}
		if id[14] != '4' {
			t.Fatalf("turn id %q 第 15 位应为 version 4", id)
		}
		switch id[19] {
		case '8', '9', 'a', 'b':
		default:
			t.Fatalf("turn id %q variant 位应为 8/9/a/b，得到 %c", id, id[19])
		}
	}
}
