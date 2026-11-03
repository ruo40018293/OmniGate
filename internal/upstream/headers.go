// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
// 规则来自 docs/api-reference.md §0/§4/§6。
package upstream

import (
	"net/http"

	"omnigate/internal/auth"
)

// 客户端身份链（R51，2026-10-22）。取值全部来自**本机官方客户端静态解包**，
// 不是照抄社区实现：
//
//   - 桌面版本：E:\WorkBuddyCN\WorkBuddyAI 的 version 文件 5.8.4
//     （resources/app.asar.unpacked/cli/product.json 的 genieVersion=5.8.4、
//     isOversea=true）；
//   - CLI 版本：同目录 cli/package.json 的 version 是占位 0.0.0，真实版本在
//     publishConfig.customPackage.version = 2.161.0（官方 resolveBundledCliVersion
//     就是这个回落口径）；
//   - UA 三段：dist/codebuddy-lite-wb.mjs 的 buildUserAgent 依次
//     unshift(productName/productVersion) → unshift(platform/platformVersion)
//     → push(userAgentExtension)，宿主 getCachedBaseEnv 给的
//     CLIENT_INFO_USER_AGENT_EXTENSION = "CLI/<cliVersion>"；
//   - X-Product：app.asar 的 buildVlmGatewayHeaders 取 deploymentType，默认 SaaS；
//   - X-IDE-Version：同一函数里的 appVersion（桌面版本，不是 CLI 版本）。
//
// 旧值 CLI/2.63.2 CodeBuddy/2.63.2 + X-IDE-Version 5.5.4 + X-Product WorkBuddy
// 是三代产品的混合身份：官方客户端绝不会这么组合，混合身份本身就是特征。
const (
	// desktopVersion 对应官方产品的 appVersion，升级客户端后必须复核（见 AGENTS.md R51）。
	desktopVersion = "5.8.4"
	// cliVersion 对应官方内置 CLI 的真实版本（package.json 的 customPackage）。
	cliVersion = "2.161.0"
	// clientUA 与 buildUserAgent 输出逐字同形："WorkBuddy/5.8.4 WorkBuddy/5.8.4 CLI/2.161.0"。
	clientUA = "WorkBuddy/" + desktopVersion + " WorkBuddy/" + desktopVersion + " CLI/" + cliVersion

	originRefererCN     = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
	// defaultClientVer 即 X-IDE-Version，官方取 appVersion。
	defaultClientVer = desktopVersion
	// defaultProduct 即 X-Product，官方取 product.json 的 deploymentType。
	defaultProduct = "SaaS"
)

func originRefererFor(a *auth.Auth) string {
	if a != nil && a.Region() == "global" {
		return originRefererGlobal
	}
	return originRefererCN
}

// CommonHeaders 设置所有 API 共享的请求头。
func CommonHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-CodeBuddy-Request", "1")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", clientUA)
	req.Header.Set("Accept-Language", acceptLanguageFor(a))
	// R51：这里原本还调了 injectAccountStableHeaders 注入 X-Machine-ID /
	// X-Session-ID（sha256 派生的 36 位裸 hex）。官方 5.8.4 客户端解包结果：
	// app.asar 里 X-Machine-ID / X-Session-ID 各出现 0 次——chat 请求根本不发，
	// X-Machine-ID 只以 CLIENT_INFO_MACHINE_ID 形式存在于宿主给 CLI 的环境变量里
	// （取值是 macOS IOPlatformUUID / 带连字符的 UUID）。少发两个头比伪造两个头
	// 更接近官方客户端，故整段删除（见 AGENTS.md R51）。
}

// ChatHeaders 在 common 之上加 chat 专属的账号头。
// 缺省字段用 X-No-* 约定（与 CodeBuddy 官方 CLI 一致）。
//
// conv / turn 是 conversationAnchors 抽出的会话锚点（见同目录 convheaders.go）。
// R91 之前这里每次现生成全部会话头，等于把一次多轮对话拆成上千个一次性会话——
// 而 X-Conversation-ID 的语义是**会话级恒定**、X-Conversation-Request-ID 是上游
// 「一次 user send = 一次请求」的计数键。
func ChatHeaders(req *http.Request, a *auth.Auth, conv, turn string) {
	CommonHeaders(req, a)
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}
	injectAttribution(req, a)
	injectConversationHeaders(req, a, conv, turn)
}

// BillingHeaders billing 接口请求头。
func BillingHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	}
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
// **调用方必须持有 a.Lock()**（本函数直接读 a.RefreshToken，不加锁——
// 若调用方未持锁，与 keepalive 刷新写回构成数据竞争）。
func RefreshHeaders(req *http.Request, a *auth.Auth) {
	CommonHeaders(req, a)
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
}

// injectAttribution 注入用量归属头——使上游调用记录显示为 WorkBuddy 桌面端。
// 取值口径见文件头 R51 注释（X-Product 取 deploymentType，X-IDE-Version 取 appVersion）。
func injectAttribution(req *http.Request, a *auth.Auth) {
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", "WorkBuddy")
	req.Header.Set("X-IDE-Type", "WorkBuddy")
	req.Header.Set("X-IDE-Version", defaultClientVer)
	req.Header.Set("X-Product", defaultProduct)
	// 官方官方类客户端 applyGatewayHeaders 的缺省头族：普通对话发这两个。
	req.Header.Set("X-Private-Data", "true")
	req.Header.Set("X-Agent-Intent", "craft")
}

// injectConversationHeaders 注入会话头族（X-Conversation-* / B3 链路）。
//
// R49（2026-10-22）：补 X-Conversation-ID。国际版上游已强制该三件套（缺任一
// 400/11-128，见 AGENTS.md R44）；国内版 10-21 起偶发 400/11-128
// "Illegal API invocation from an unapproved channel"，直发对照实验未能钉死触发条件，
// 但 12 发带该头全部 200，证明**无害**；两区同源 API 的校验只可能趋同，先对齐即把
// 已知的最后一个头差异关掉。
//
// R91（2026-11-03）：取值语义修正。此前每次现生成三件套（X-Conversation-ID 也每请求变），
// 按官方 5.8.4 抓包应为「会话级恒定 / 轮级一次 send / 调用级每请求」。取值生成见
// convheaders.go，本函数只负责落头。
func injectConversationHeaders(req *http.Request, a *auth.Auth, conv, turn string) {
	uid := ""
	if a != nil {
		uid = a.UID
	}
	msgID := newMessageID()
	turnID := turnIDOf(uid, conv, turn)
	req.Header.Set("X-Conversation-ID", conversationID(uid, conv))
	req.Header.Set("X-Conversation-Request-ID", turnID)
	req.Header.Set("X-Conversation-Message-ID", msgID)
	req.Header.Set("X-Request-ID", msgID)
	req.Header.Set("X-Root-Request-ID", turnID)
	// B3 是**每请求**链路追踪头，跟调用级 id 走（不是会话级）。
	req.Header.Set("X-B3-TraceId", msgID)
	req.Header.Set("X-B3-SpanId", msgID[:16])
	req.Header.Set("X-B3-Sampled", "1")
}

func acceptLanguageFor(a *auth.Auth) string {
	if a != nil && a.Region() == "global" {
		return "en-US"
	}
	return "zh-CN"
}
