// identity_headers_test.go R51 回归：客户端身份链对齐官方 5.8.4 桌面客户端，
// 并守住两区 + 登录侧的身份串一致性。
//
// 为什么需要这组测试：R51 改的是一条**混合身份**——旧出站组合
// （UA=CLI/2.63.2 CodeBuddy/2.63.2 + X-IDE-Version=5.5.4 + X-Product=WorkBuddy）
// 分属 CodeBuddy CLI / 若干代桌面版本 / 产品链三个不同代次，官方客户端绝不会
// 这么组合，混合身份本身就是可识别特征。取值依据见 headers.go 的 R51 注释
// （本机 E:\WorkBuddyCN\WorkBuddyAI 官方 5.8.4 客户端静态解包）。
//
// 纪律：版本号是**快照**。官方客户端升级后必须按 version 文件 +
// cli/package.json 的 customPackage 复核本文件的常量，不能靠猜新版本号。
package upstream

import (
	"net/http"
	"strings"
	"testing"

	"omnigate/internal/auth"
)

// TestIdentityHeadersMatchOfficialDesktop 逐字锁死出站身份链。
// 任一项改回旧值（或某天有人"顺手"调版本号）都必须变红。
func TestIdentityHeadersMatchOfficialDesktop(t *testing.T) {
	req, err := http.NewRequest("POST", "https://chat.example/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	ChatHeaders(req, &auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.codebuddy.cn"})

	// UA 三段与官方 buildUserAgent 输出同形：
	// <productName>/<appVersion> <productName>/<appVersion> CLI/<cliVersion>
	const wantUA = "WorkBuddy/5.8.4 WorkBuddy/5.8.4 CLI/2.161.0"
	if got := req.Header.Get("User-Agent"); got != wantUA {
		t.Errorf("User-Agent=%q，官方 5.8.4 是 %q（R51）", got, wantUA)
	}
	if got := req.Header.Get("X-IDE-Version"); got != "5.8.4" {
		t.Errorf("X-IDE-Version=%q，官方取 appVersion=5.8.4（R51）", got)
	}
	if got := req.Header.Get("X-Product"); got != "SaaS" {
		t.Errorf("X-Product=%q，官方取 product.json 的 deploymentType=SaaS（R51）", got)
	}
	// 普通对话的官方缺省归属头。
	if got := req.Header.Get("X-Private-Data"); got != "true" {
		t.Errorf("X-Private-Data=%q，官方普通对话为 true（R51）", got)
	}
	if got := req.Header.Get("X-Agent-Intent"); got != "craft" {
		t.Errorf("X-Agent-Intent=%q，官方缺省 craft（R51）", got)
	}
}

// TestNoSelfInventedFingerprintHeaders：官方客户端 chat 不发 X-Machine-ID /
// X-Session-ID（app.asar 里各出现 0 次），本仓曾自造 36 位 bare hex 发出去。
// 少发两个头比伪造两个头更接近官方客户端。
func TestNoSelfInventedFingerprintHeaders(t *testing.T) {
	req, err := http.NewRequest("POST", "https://chat.example/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	ChatHeaders(req, &auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.codebuddy.cn"})
	for _, k := range []string{"X-Machine-ID", "X-Session-ID"} {
		if v := req.Header.Get(k); v != "" {
			t.Errorf("%s=%q 不应出现在 chat 请求里：官方客户端不发（R51）", k, v)
		}
	}
}

// TestLoginSideUsesSameIdentity：登录与聊天必须是同一个客户端身份，
// 否则同一账号在两条链路上呈现两种产品形态（登录走 CLI 形、聊天走桌面形）。
func TestLoginSideUsesSameIdentity(t *testing.T) {
	// 直接断言包内常量，避免引入 login 包的循环依赖（login → upstream 不存在，
	// 但 upstream → login 也不该有；用文本级护栏在 parity 测试里做跨包检查）。
	req, _ := http.NewRequest("POST", "https://chat.example/v2/chat/completions", nil)
	ChatHeaders(req, &auth.Auth{AccessToken: "at", UID: "u1"})
	ua := req.Header.Get("User-Agent")
	if !strings.Contains(ua, "CLI/"+cliVersion) {
		t.Errorf("UA %q 必须含 CLI/%s 段：/v3/config 有 \"CLI 形 UA\" 门禁（400/12403）", ua, cliVersion)
	}
	if !strings.Contains(ua, "WorkBuddy/"+desktopVersion) {
		t.Errorf("UA %q 必须含 WorkBuddy/%s 段（R51）", ua, desktopVersion)
	}
}
