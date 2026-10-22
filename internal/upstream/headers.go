// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
// 规则来自 docs/api-reference.md §0/§4/§6。
package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"

	"omnigate/internal/auth"
)

const (
	clientUA            = "CLI/2.63.2 CodeBuddy/2.63.2"
	originRefererCN     = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
	defaultClientVer    = "5.5.4"
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
	injectAccountStableHeaders(req, a)
}

// ChatHeaders 在 common 之上加 chat 专属的账号头。
// 缺省字段用 X-No-* 约定（与 CodeBuddy 官方 CLI 一致）。
func ChatHeaders(req *http.Request, a *auth.Auth) {
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
	injectConversationHeaders(req, newMessageID())
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
func injectAttribution(req *http.Request, a *auth.Auth) {
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", "WorkBuddy")
	req.Header.Set("X-IDE-Type", "WorkBuddy")
	req.Header.Set("X-IDE-Version", defaultClientVer)
	req.Header.Set("X-Product", "WorkBuddy")
}

// injectConversationHeaders 注入会话头族（X-Conversation-Request-ID / B3 链路）。
//
// R49（2026-10-22）：补 X-Conversation-ID。国际版上游已强制该三件套（缺任一
// 400/11-128，见 AGENTS.md R44）；国内版 10-21 起偶发 400/11128
// "Illegal API invocation from an unapproved channel"（11h 内 3 次、随后自愈，
// 灰度特征），直发对照实验未能钉死触发条件——但 12 发带该头全部 200，
// 证明**无害**；两区同源 API 的校验只可能趋同，先对齐即把已知的最后一个
// 头差异关掉。真正的定案数据靠 OMNIGATE_CAPTURE_CHAT 捕获下次复现（见 client.go）。
func injectConversationHeaders(req *http.Request, messageID string) {
	cid := newMessageID()
	req.Header.Set("X-Conversation-ID", messageID)
	req.Header.Set("X-Conversation-Request-ID", cid)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", messageID)
	req.Header.Set("X-Root-Request-ID", cid)
	req.Header.Set("X-B3-TraceId", messageID)
	req.Header.Set("X-B3-SpanId", messageID[:16])
	req.Header.Set("X-B3-Sampled", "1")
}

func newMessageID() string {
	var b [16]byte
	n := time.Now().UnixNano()
	for i := 0; i < 8; i++ {
		b[i] = byte(n >> (i * 8))
		b[15-i] = byte(n >> ((7 - i) * 8))
	}
	return hex.EncodeToString(b[:])
}

func acceptLanguageFor(a *auth.Auth) string {
	if a != nil && a.Region() == "global" {
		return "en-US"
	}
	return "zh-CN"
}

// injectAccountStableHeaders 注入按 uid 稳定派生的 X-Machine-ID / X-Session-ID（36 hex）。
func injectAccountStableHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || a.UID == "" {
		return
	}
	req.Header.Set("X-Machine-ID", deriveStableID(a.UID, "machine"))
	req.Header.Set("X-Session-ID", deriveStableID(a.UID, "session"))
}

func deriveStableID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18])
}
