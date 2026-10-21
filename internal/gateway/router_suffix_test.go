package gateway

// router_suffix_test.go 倍率后缀与 model_map 的交互。
//
// ## 为什么单独测这里
//
// 模型名解析有两层（gateway.Router.Resolve → server.runtimeForModel），
// 而 `Map`（用户配置的裸名 → 渠道/模型 映射）**只在前一层查**。
// 若不剥后缀，`Map["glm-5.3-flash (0.06x)"]` 查不到 → 静默落到 compat.default_channel。
//
// 后果是「静默走错渠道」：用户把映射配成 qodercn，请求却发到了 workbuddy，
// 且响应正常（200），没有任何错误提示 —— 这类故障极难自查。

import (
	"testing"

	"omnigate/internal/provider"
)

// TestResolveStripsSuffixBeforeMapLookup 后缀不得让映射失效。
func TestResolveStripsSuffixBeforeMapLookup(t *testing.T) {
	rt := &Router{
		Default:  "workbuddy", // 兜底渠道与映射目标故意不同，便于发现「落到兜底」
		Channels: []string{"workbuddy", "qodercn"},
		Map:      map[string]string{"glm-5.3-flash": "qodercn/glm-5.3-flash"},
	}

	cases := []struct{ in, want string }{
		{"glm-5.3-flash", "qodercn/glm-5.3-flash"},
		{"glm-5.3-flash (0.06x)", "qodercn/glm-5.3-flash (0.06x)"},
		{"glm-5.3-flash(0.06x)", "qodercn/glm-5.3-flash (0.06x)"},  // 丢空格也认
		{"glm-5.3-flash (1.83X)", "qodercn/glm-5.3-flash (1.83x)"}, // 大写 X 归一
	}
	for _, c := range cases {
		got, err := rt.Resolve(c.in)
		if err != nil {
			t.Errorf("Resolve(%q) 报错：%v", c.in, err)
			continue
		}
		// 只比渠道归属（后缀由下游再剥，这里比全串会被格式差异干扰）
		wantCh, _ := provider.StripRateSuffix(c.want)
		gotCh, _ := provider.StripRateSuffix(got)
		if gotCh != wantCh {
			t.Errorf("Resolve(%q)=%q，渠道应为 %q（后缀导致映射失效 → 落到兜底渠道）", c.in, got, wantCh)
		}
	}
}

// TestResolveKeepsSuffixOnPrefixedModel 已带前缀的名字：后缀保留但不被误剥成路由问题。
func TestResolveKeepsSuffixOnPrefixedModel(t *testing.T) {
	rt := &Router{Default: "workbuddy", Channels: []string{"workbuddy", "traework"}}

	got, err := rt.Resolve("workbuddy/hy3 (0.06x)")
	if err != nil {
		t.Fatalf("报错：%v", err)
	}
	bare, had := provider.StripRateSuffix(got)
	if !had || bare != "workbuddy/hy3" {
		t.Fatalf("剥后缀后=%q(had=%v)，want workbuddy/hy3", bare, had)
	}
}

// TestResolveSuffixStillValidatesChannel 剥后缀不能放宽「未知渠道」的校验。
func TestResolveSuffixStillValidatesChannel(t *testing.T) {
	rt := &Router{Default: "workbuddy", Channels: []string{"workbuddy"}}
	for _, in := range []string{"nosuchchan/x (0.06x)", "nosuchchan/x"} {
		if _, err := rt.Resolve(in); err == nil {
			t.Errorf("Resolve(%q) 应报「未知渠道」", in)
		}
	}
}

// TestResolveSuffixOnWildcardMap 通配映射同样不能被后缀绕过。
//
// 注意断言：通配映射会把**整个名字**改写成映射值里的字面模型名
// （`claude-sonnet-4` → `claude-native`），这是既有语义，不是本特性引入的。
func TestResolveSuffixOnWildcardMap(t *testing.T) {
	rt := &Router{
		Default:  "workbuddy",
		Channels: []string{"workbuddy", "qoder"},
		Map:      map[string]string{"claude-*": "qoder/claude-native"},
	}
	got, err := rt.Resolve("claude-sonnet-4 (1.20x)")
	if err != nil {
		t.Fatalf("报错：%v", err)
	}
	bare, _ := provider.StripRateSuffix(got)
	if bare != "qoder/claude-native" {
		t.Fatalf("got=%q，通配映射应命中 qoder/claude-native（否则说明后缀绕过了映射，落到兜底 workbuddy）", got)
	}
	// 不带后缀时得到同一个渠道 —— 有无后缀必须解析到同一处
	got2, _ := rt.Resolve("claude-sonnet-4")
	bare2, _ := provider.StripRateSuffix(got2)
	if bare != bare2 {
		t.Fatalf("带后缀解析到 %q，裸名解析到 %q，应一致", bare, bare2)
	}
}

// TestResolveErrorMessagesKeepOriginalName 报错信息里应回显用户**原始**输入
// （含倍率），否则用户看到 `model "glm-5.3-flash"` 会困惑「我明明传了带倍率的」。
func TestResolveErrorMessagesKeepOriginalName(t *testing.T) {
	rt := &Router{Channels: []string{"workbuddy"}} // 无 Default、无 Map → 裸名必报错
	_, err := rt.Resolve("glm-5.3-flash (0.06x)")
	if err == nil {
		t.Fatal("应报错")
	}
	if want := "glm-5.3-flash (0.06x)"; !contains(err.Error(), want) {
		t.Errorf("报错信息应含原始模型名 %q，实际：%v", want, err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
