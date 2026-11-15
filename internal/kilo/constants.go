// Package kilo 接入 Kilo AI 网关的**免费池**（api.kilo.ai/api/gateway）。
//
// 与其它渠道根本不同的三点：
//
//  1. **免密**：没有登录、没有 API Key、没有可伪造的账号凭据。请求不带
//     Authorization，免费范围完全由上游清单里的 `isFree === true` 决定。
//     因此本渠道没有 login_kilo 包、没有 auth 加载器、不进 reloadAccounts，
//     由 main 在装配时注入唯一虚拟账号（同 R16 的无账号渠道形态，R22 的虚拟账号约束）。
//  2. **免费集合只认上游声明**：本地不按模型名猜（不以 `:free` 后缀、不以价格字段
//     推断）。猜错的两种后果都很难看——把付费模型当免费会让每次调用都拿到
//     `PAID_MODEL_AUTH_REQUIRED`，把免费模型漏掉则是白丢可用模型。
//  3. **上游排队时下发 `: KILO PROCESSING` 注释行保活**：通用 SSE 解析会跳过注释行，
//     所以它不会被当成内容，也**绝不能**把它当 idle 超时证据——那是上游在告诉我们
//     「我还在排队，别断」。
package kilo

import "time"

const (
	// Kind 渠道标识，同时是模型名前缀（kilo/...）。
	Kind = "kilo"

	// DefaultBase 网关基址。
	DefaultBase = "https://api.kilo.ai/api/gateway"

	// AnonymousUID 虚拟账号的固定 UID。用固定值而非随机值，是为了让
	// data/state-kilo.json 与统计口径在重启后保持连续（换 UID 会让历史行变成孤儿）。
	AnonymousUID = "kilo-anonymous"

	// AnonymousName 虚拟账号在面板上的显示名。
	AnonymousName = "免费池"

	// userAgent 出站 UA。上游没有像 workbuddyai 那样做 UA 门禁（实测 200），
	// 但仍显式声明——默认的 Go-http-client/1.1 是裸奔指纹（R91 的教训）。
	userAgent = "OmniGate"

	// noExpiry 虚拟账号的到期时刻（2100-01-01）。
	// **不能为 0**：auth.NeedsRefreshLocked 对 ExpiresAt<=0 恒返回 true（R22），
	// 会让我们每轮都去「刷新」一个根本没有刷新端点的账号。
	noExpiry int64 = 4102444800

	// requestTimeout 非流式请求（清单/无流对话）的整体上限。
	requestTimeout = 2 * time.Minute
	// listTimeout 拉模型清单的上限：清单失败要快速退回静态兜底，不能拖住面板刷新。
	listTimeout = 20 * time.Second

	// EpChat 对话端点（OpenAI 兼容）。
	EpChat = "/chat/completions"
	// EpModels 模型清单端点。
	EpModels = "/models"
)
