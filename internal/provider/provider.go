// Package provider 定义不同上游（workbuddy / traework）共用的最小接口。
// 只抽取 server/scheduler 必需能力，避免为未来平台过度设计。
package provider

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"omnigate/internal/auth"
)

// Kind 平台标识，同时也是模型名前缀。
type Kind string

const (
	WorkBuddy   Kind = "workbuddy"
	WorkBuddyAI Kind = "workbuddyai" // WorkBuddy 国际版（www.workbuddy.ai），与国内版完全独立
	TraeWork    Kind = "traework"
	Qoder       Kind = "qoder"    // QoderWork（qoder.com.cn，移植自 qoderwork2api）
	QoderCN     Kind = "qodercn"  // QoderCN（qoder.com.cn，移植自 qoder2api，独立渠道）
	QoderCOM    Kind = "qodercom" // QoderCOM 国际版（qoder.com / qoder.sh，移植自 qodercn）
	QwenWork    Kind = "qwenwork" // 千问办公（gateway.qwenwork.cn + qwenwork.cn）
	// MonkeyCode 平台托管模型（proxy.monkeycode-ai.com）：凭据由面板从本机官方客户端导入，
	// 一个账号 = oma_ api_key + omas_ signing_secret；上游无目录/额度/刷新接口，
	// 故模型表静态、额度恒 0、RefreshToken 为空实现。
	MonkeyCode Kind = "monkeycode"
	// Raccoon 小浣熊（xiaohuanxiong.com）：凭据由面板从本机官方客户端导入，
	// access_token 约 2h + refresh_token 约 30d，可用 refresh_token 自动续期。
	Raccoon Kind = "raccoon"
	// Loomy 讯飞 Loomy（loomyad.xunfei.cn）：凭据由面板从本机官方客户端导入，
	// session 约 14 天且上游无 refresh 端点，到期需重新导入。
	Loomy    Kind = "loomy"
	TraeCode Kind = "traecode" // Trae 代码版：与 TraeWork 同一上游、共用账号，function=solo_agent
	GLM      Kind = "glm"      // 智谱清言（chatglm.cn 网页版私有接口，凭据为 chatglm_refresh_token）
	// Minimax MiniMax Code 中国版（agent.minimax.cn）：OAuth 设备码 + PKCE 登录，
	// access_token 前缀 mmoat_（**不是 JWT**，过期只能由 expires_in 自算），
	// 推理走 Anthropic Messages 形态，且有真·每日签到（signin/status + signin/claim）。
	Minimax Kind = "minimax"
	// Cline Cline 官方 API（api.cline.bot）：粘贴型渠道，凭据是官方文档化的
	// sk_ + 64 hex（只在创建时显示一次，**不过期、不轮换** ⇒ 无 refresh、无保活、无签到）。
	// 免费范围只认上游 recommended-models 的 free[]，不做任何本地假设。
	Cline Kind = "cline"

	// Gemini Google Code Assist / Cloud Code（daily-cloudcode-pa.googleapis.com）：
	// 正常多账号 OAuth 渠道（可 refresh）⇒ **不置** SingleAccount，账号级错误照常轮换。
	// 鉴权是 Bearer + 逐字身份五头（不带 x-goog-api-key），信封逐层字母序序列化；
	// quota / loadCodeAssist 固定走 sandbox 端点；配额是「窗口剩余百分比」，无签到。
	// 凭据扩展字段（cloudaicompanionProject / paid_tier*）落 auth.Extra。
	Gemini Kind = "gemini"

	// ZCode 智谱 ZCode：Anthropic Messages 形态；请求体 `system` 必须内嵌官方
	// 身份块（否则 405 / code 3012），身份头族缺 X-Device-Mid 直接 400 code 3001；
	// 凭据不可续期（无 refresh 端点，ExpiresAt 写远期哨兵）。
	// LobsterAI 有道龙虾（lobsterai-server.youdao.com）：OAuth 三件套可续期，
	// 走 Anthropic Messages 形态；上游 ID 自带命名空间（claude-* / gpt-* 等
	// 原样透传，不走 <kind>/ 拼装）。
	LobsterAI Kind = "lobsterai"
	ZCode     Kind = "zcode"
	// Kilo Kilo AI 网关免费池（api.kilo.ai/api/gateway）：**免密**渠道，
	// 没有登录/API Key/可伪造凭据，免费集合只认上游清单的 isFree 布尔；
	// 无签到、无积分、无解析端点 ⇒ 虚拟账号（R22）+ SingleAccount（R23）。
	Kilo Kind = "kilo"

	// OfficeAce 华为云果办模型网关（agentarts）：OpenAI 兼容协议，但有三处改写——
	// 恒以 stream:true 打上游（非流式本地聚合）、按模型补足 max_tokens 推理预算、
	// 剥离 glm-5.3 系不认的 thinking 参数。上游限 1 req/s（429 ModelArts.81101），
	// Client.ChatThrottleInterval 默认 1.1s 节流。
	// 凭据：网关 Basic（存 AccessToken）+ STS 临时 AK/SK/SecurityToken + DPoP 私钥，
	// 后三者全在 Extra；STS 过期靠 refresh.go 的 ensureCredential 续。
	// **有真签到**：POST {gateway}/v1/subscription/bonus/claim（V11 签名，body {}）。
	OfficeAce Kind = "officeace"

	// CodeArts 华为云 CodeArts Agent（snap-access.cn-north-4）：Anthropic Messages 形态
	// 转华为自有 DSML 工具调用语法。凭据是**华为云 STS 临时 AK/SK/SecurityToken**
	// （不是 Bearer）：accessToken 承载 AK，refreshToken 承载 OAuth refresh_token 用于
	// 静默续期，secret_access_key / security_token / dpop_private_key_jwk /
	// domain_id / user_id / project_id / region 全在 Extra。签名用 AK/SK + DPoP。
	// **有签到**：benefit（免费额度）模型目录 + 每日额度领取，见 credits.go。
	CodeArts Kind = "codearts"
)

func (k Kind) String() string { return string(k) }

// ModelNamespaces 声明「上游 ID 自带命名空间」的渠道：命名空间前缀 → 渠道 kind。
//
// 这类渠道对客户端**直接透传上游 ID**，不再拼 `<kind>/` 前缀（首例为 cline
// 免费档：上游 free[] 的 ID 就是 `cline-free/*`）。同时旧的 `<kind>/<上游ID>` 双层写法
// 继续有效 —— 老配置/老客户端里的映射表与降级链都是那种写法，不能失效。
var ModelNamespaces = map[string]Kind{
	"cline-free": Cline,
}

// PublicModelID 返回客户端应填写的模型 ID（面板复制 / `/v1/models` / 报错
// 提示同源——三处不一致会让用户「复制了却拿不到」）。
// 上游 ID 自带命名空间时直接透传上游 ID；其余渠道拼 `<kind>/<上游ID>`。
func PublicModelID(k Kind, upstreamID string) string {
	up := strings.TrimSpace(upstreamID)
	if i := strings.IndexByte(up, '/'); i > 0 {
		if kk, ok := ModelNamespaces[up[:i]]; ok && kk == k {
			return up
		}
	}
	return k.String() + "/" + up
}

// ErrKind 错误分类，驱动 pool 冷却状态机。
type ErrKind int

const (
	ErrNone           ErrKind = iota // 成功
	ErrHardCredit                    // 余额/权益不足 → 长冷却
	ErrSoftRate                      // 429 软限流 → 短冷却
	ErrSessionDead                   // 登录态失效 → 禁用
	ErrNotFound                      // 404 上游偶发 → 短冷却不累计 errCount
	ErrServer                        // 5xx 上游故障
	ErrClient                        // 其他 4xx / 业务错误
	ErrContentBlocked                // 内容策略拦截（400 + 审核文案）→ 不罚账号，透传原文
	ErrPromptTooLong                 // 11115 上下文超限 → 请求级错误，不罚号不轮转，透传原文
	ErrImageInvalid                  // 图片格式/数据无效（11135 等）→ 请求级错误，不罚号不轮转，透传原文
	ErrBadParams                     // 出站 body / 请求参数无法被上游接受 → 请求级错误，不罚号不轮转，透传原文
	ErrWafBlock                      // 403 + 非业务信封（WAF 拦截页/空体）→ 账号软冷却
	ErrAccountFault                  // 账号级授权/配额故障（11140/14017）→ 冷却轮换
	ErrModelBlocked                  // 11102 该后端无此模型 → (账号,模型) 负缓存避让
	ErrPassthrough                   // 请求级拒绝（形态/地域/身体不被接受）→ 原文透传，不罚账号不轮转
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	case ErrContentBlocked:
		return "content_blocked"
	case ErrPromptTooLong:
		return "prompt_too_long"
	case ErrImageInvalid:
		return "image_invalid"
	case ErrBadParams:
		return "bad_params"
	case ErrWafBlock:
		return "waf_block"
	case ErrAccountFault:
		return "account_fault"
	case ErrModelBlocked:
		return "model_blocked"
	case ErrPassthrough:
		return "passthrough"
	default:
		return "none"
	}
}

// Rotatable 报告该错误是否应换下一个账号重试本请求（账号级错误）。
// 账号有问题的（限流/欠费/登录态死/偶发 404/上游 5xx/账号故障）：换号有价值；
// 请求内容有问题的（内容拦截/上下文超限/模型不存在）：换号必然复现，透传原文结束；
// ErrClient（其他 4xx 业务错误）默认可换一次——部分渠道业务错误挂在账号权益上。
func (k ErrKind) Rotatable() bool {
	switch k {
	case ErrSoftRate, ErrHardCredit, ErrSessionDead, ErrNotFound, ErrServer, ErrAccountFault, ErrClient:
		return true
	default:
		return false
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// ModelInfo 动态/静态模型信息。
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64
	MaxTokens     int64
	// ContextFromAPI 标记 ContextWindow/MaxTokens 是否来自上游接口。
	// false 表示是硬编码估算/占位（如渠道不返回该字段），
	// 消费方（面板/API）不应把估值当作真实容量展示。
	ContextFromAPI bool
	// HandMeasured 标记 ContextWindow/能力字段是**本工具手工实测**得出的
	// （非上游下发、也非拍脑袋估算）。首例：cline 的 measuredCaps ——
	// 上游 recommended-models 完全不返回上下文/能力字段，只能逐个实测。
	//
	// 与 ContextFromAPI 的关系：两者互斥，同置时以 ContextFromAPI 为准。
	// 为什么单独一个标志：口径不同 —— 实测值不是「上游声明」，面板展示时
	// 必须能如实标注来源，否则就是把实测冒充成官方值。
	HandMeasured bool
	// ContextFromUser 标记上下文/能力字段是**用户在面板手填**的（自定义渠道专用）：
	// 既不是上游声明，也不是本工具实测，但同样是「有人对它负责」的确定值。
	ContextFromUser bool
	// ContextWindows 上游声明的上下文档位表（升序，典型 200000 / 1000000）。
	// 只有上游真的给出 context_config 这类档位表时才有值；空 = 只声明单一窗口
	// （ContextWindow）或完全不声明。消费方不得自行编造档位——与 ReasoningEfforts
	// 同一哲学：声明即事实，没声明就不许替上游做主。
	//
	// 与 ContextWindow 的关系：ContextWindow 是**广告值**（取最大档，宁高勿低），
	// ContextWindows 是**可选项**；两者来自同一份上游数据，不互相推导。
	ContextWindows []int64
	// 以下能力字段均为「已确认为 true」才置 true；数据缺失时保持 false（未知）。
	// 未知与「明确不支持」在语义上不同，但对消费方（/v1/models 声明、UI 图标）
	// 的处理一致：不声明该能力。需要区分时由各渠道自行记录来源。
	SupportsImages    bool // 支持图像输入（多模态视觉）
	SupportsReasoning bool // 支持思考/推理模式
	SupportsTools     bool // 支持函数/工具调用
	// ReasoningEfforts 上游目录显式声明的思考档位表（典型 low/medium/high/xhigh/max）。
	// nil = 上游未声明，消费方不得自行编造（对齐 ContextFromAPI 哲学）。
	ReasoningEfforts []string
	// DefaultReasoningEffort 上游声明的默认档位；空 = 未声明。
	DefaultReasoningEffort string
	// CanDisableThinking 该模型思考是否可关闭；nil = 上游未声明。
	// 上游只对显式 true 语义不同（false 可能是「不可关闭」也可能是「未声明」），
	// 因此保留原始三态。
	CanDisableThinking *bool
}

// CleanEfforts 规范化思考档位表：去空白、去空串、去重（保序）。
// 上游目录常带重复项或空串，直接透传会让 /v1/models 的 supported_parameters
// 出现 "reasoning_effort" 之外的噪声，故统一清洗。全空时返回 nil（= 未声明）。
func CleanEfforts(raw []string) []string {
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, s := range raw {
		if s = strings.TrimSpace(s); s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// InputModalities 按 OpenAI 生态惯例给出输入模态列表（OpenRouter/llama.cpp 的
// architecture.input_modalities 语义）。不支持图像时只返回 ["text"]。
func (m ModelInfo) InputModalities() []string {
	if m.SupportsImages {
		return []string{"text", "image"}
	}
	return []string{"text"}
}

// Modality 返回 OpenRouter 风格的 modality 描述串（如 "text+image->text"）。
func (m ModelInfo) Modality() string {
	if m.SupportsImages {
		return "text+image->text"
	}
	return "text->text"
}

// ModelPricing 模型积分定价（从上游 API 拉取）。
type ModelPricing struct {
	Model   string  `json:"model"`
	Channel string  `json:"channel"`
	Rate    float64 `json:"rate"`
	Note    string  `json:"note,omitempty"` // 促销/标签文案（已剔除颜色）
	// Color 是上游 badge 附带的颜色（形如 "#FF0000"），无则空。
	Color string `json:"color,omitempty"`
	// Explicit 标记 Rate 是否来自上游显式倍率字段。
	// 例如 CN 的 auto 模型根本没有 credits 字段，其 Rate 值无意义，
	// Explicit=false 时不应当作「免费」展示（应为未知）。
	// 零值兼容：老缓存/老实现未设置时视为 true（保持原有行为）。
	Explicit *bool `json:"explicit,omitempty"`
}

// IsExplicit 报告该定价是否来自上游显式倍率字段（未设置时按 true 处理）。
func (p ModelPricing) IsExplicit() bool {
	return p.Explicit == nil || *p.Explicit
}

// Upstream 是 server/scheduler 依赖的最小上游能力集合。
type Upstream interface {
	RefreshToken(a *auth.Auth) error
	ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error)
	FetchModels(a *auth.Auth) ([]ModelInfo, error)
	FetchModelPricing(a *auth.Auth) ([]ModelPricing, error)
	UserResource(a *auth.Auth) (int64, error)
	// UserResourceDetail 返回可消耗余额 + 明细条目（含到期时间与可用性标记）。
	// 返回的 remain 口径与 UserResource 一致，均为「本工具可消耗」的余额，
	// 不得包含不可用池——否则 pool 会按虚高余额选号。
	UserResourceDetail(a *auth.Auth) (int64, []ResourceItem, error)
	DailyCheckin(a *auth.Auth) error
	Classify(status int, body string) ErrKind

	// Stream/Aggregate 的 model 参数是「客户端请求的原始模型名」（含 channel/ 前缀），
	// 由调用方显式传入而非渠道内部记忆状态——后者在多账号并发下会串号。
	// 实现方应在输出的 model 字段回填该值（上游常返回 "auto" 或裸名）。
	// Stream 返回值为末帧捕获的 usage（OpenAI 形状，pt/ct/total），供 handler 记 token 流水；
	// 上游未返回 usage 时为 nil（调用方记 0 token + 请求数）。
	Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error)
	Aggregate(r io.Reader, model string) (map[string]any, error)
}

// ProgressSink 是**可选**能力：渠道若能在「上游还没吐内容」的等待期上报进度
// （排队前方人数、第几跳重投、零内容断开等），实现 SetProgressSink 即可；
// handler 用匿名接口断言探测，不实现则完全零影响（无需改这十几个渠道的现有实现）。
//
// 存在理由（R56）：qodercn 排队时上游只回保活碎片、约 180s 后零内容断开，网关在
// 内层保活重投期间不向客户端发任何字节，客户端界面只剩一个计时器；用户体感是
// 「卡住了，只能挨个重启再试」。把等待期状态暴露出来是最小代价的解法。
//
// 并发语义（重要）：同一个渠道 Client 会被多个请求**并发**使用，所以回调必须按
// 请求隔离（用 body 作键），不能是单一全局字段——否则 A 请求的排队信息会挂到
// B 请求的面板行上。返回的 cancel 用于请求结束时注销，避免登记表无限增长。
type ProgressSink interface {
	SetProgressSink(body []byte, fn func(Progress)) (cancel func())
}

// Progress 一次等待期进度事件。
type Progress struct {
	Phase  string // "queued" / "retrying" / "streaming"
	Pos    int    // 上游给出的队列前方人数（未知为 0）
	Wait   int    // 上游给出的预计等待秒数（未知为 0）
	Detail string // 人类可读一句话（进面板事件列表）
}

// StreamErrorClassifier 由「流内业务错误可分类」的渠道实现（当前 traework 的
// SOLOStreamError）。handler 用 errors.As 取值，据此决定账号惩罚——
// 使 server 层不必 import 具体渠道包。
//
// 为什么需要它：流式请求的 HTTP 200 与响应头在读到上游错误帧**之前**就已发出，
// 协议上无法在同一请求内换号重试。唯一可行的补救是「把中招账号冷却掉」，
// 让客户端**下一次**重试时由挑号逻辑换到别的账号（3004 是账号级限流，换号有效）。
type StreamErrorClassifier interface {
	Kind() ErrKind
}

// CheckinReporter 由「签到结果可结构化上报」的渠道实现（当前 QoderCN / QoderCOM）。
// 调度器优先使用它；未实现的渠道回退 DailyCheckin + isAlready 文本判定。
// 之所以需要独立接口：error 通道无法区分 no_campaign 与 error——两者都不是
// 「已签到」，却都需要在当日窗口内继续重试，而 error 文本判定做不到。
type CheckinReporter interface {
	DailyCheckinReport(a *auth.Auth) (CheckinReport, error)
}

// CheckinGranter 由「签到回执自带发放额」的渠道实现（当前 WorkBuddy，issue #67）。
// 上游 daily-checkin 回执的 credit/today_credit 是本次发放的权威值；调度器在签到
// 成功后用它直接记 earn（ledger.RecordCheckinEarn），不再完全依赖快照差分——
// WorkBuddy 签到包落在「明日到期」key 上且提前一天以 r=0 建档，「发放即消耗」
// 场景下差分会把当日 earn 抵消为 0（面板今日收入显示 0 的根因）。
// granted<=0 时调度器静默降级回差分口径；未实现本接口的渠道行为不变。
type CheckinGranter interface {
	DailyCheckinGrant(a *auth.Auth) (granted int64, err error)
}

// CheckinStatus 是签到结果状态（语义对齐上游 qoder2api 的 checkinStatus*）。
// 决定调度器的「当日是否已完成」判定与窗口内重试策略。
type CheckinStatus string

const (
	// CheckinClaimed 本次真正领取成功（金额可能为 0：上游未回填 benefit）。
	CheckinClaimed CheckinStatus = "claimed"
	// CheckinAlready 今日已领取（幂等：409 / CLAIMED / replayed）。
	CheckinAlready CheckinStatus = "already_claimed"
	// CheckinNoCampaign 无可用签到活动（含 legacy DISABLED）：可能是活动尚未创建，
	// 属于「可重试」状态——上游 10:00 整点存在延迟。
	CheckinNoCampaign CheckinStatus = "no_campaign"
	// CheckinNoToken 本地无可用凭据（可重试，refresh 后可能恢复）。
	CheckinNoToken CheckinStatus = "no_token"
	// CheckinError 其它失败（可重试）。
	CheckinError CheckinStatus = "error"
)

// Retryable 报告该状态是否应在当日窗口内继续重试。
// 只有「已领取」和「本次领取成功」算完成——上游同款判定。
func (s CheckinStatus) Retryable() bool {
	switch s {
	case CheckinClaimed, CheckinAlready:
		return false
	default:
		return true
	}
}

// CheckinReport 单次签到的结构化结果（Upstream.DailyCheckinReport 返回值）。
type CheckinReport struct {
	Status CheckinStatus // 结果状态
	Msg    string        // 展示文案（含金额），可为空
	Amount int64         // 本次领取金额（仅 Status==CheckinClaimed 时有意义）
}

// ResourceItem 积分明细条目。
type ResourceItem struct {
	Name   string `json:"name"`
	Total  int64  `json:"total"`
	Used   int64  `json:"used"`
	Remain int64  `json:"remain"`

	// ExpireAt 该条目到期时刻（RFC3339，UTC+8 墙钟）。空串表示上游未下发到期时间，
	// 前端据此隐藏「有效期」列——不得用零值时间冒充「永不过期」。
	ExpireAt string `json:"expire_at,omitempty"`
	// Key 条目稳定标识（上游提供的 ID，如 TraeWork entitlement_id），
	// 供 ledger 差分对账用；渠道无 ID 时留空，差分退回 Name 作伪键。
	Key string `json:"key,omitempty"`
	// Usable 标记该条目是否属于本工具可消耗的额度池。
	// TraeWork 存在按 available_endpoint 划分的专用池（ep=1，官方客户端专用），
	// 本工具走的是 ep=0；这类额度对用户是「看得见用不了」，需在界面上分开统计。
	// 注意：零值为 false，故各渠道构造时须显式置位；渠道无此概念时统一填 true。
	Usable bool `json:"usable"`
	// InfoOnly 标记该条目**只作展示**，不参与任何积分算术（Summarize 小计、
	// ExpiringWithin 临期、ledger 差分）。用于「同一账号下计量单位不同的另一套额度」——
	// 如 MonkeyCode 的每日 Token 额度（单位是 token，而积分是 credits）：
	// 两者都该显示，但相加无意义。
	// 注意：InfoOnly 条目应同时置 Usable=true（它在界面上既非「不可用」，也不该被小计）。
	InfoOnly bool `json:"info_only,omitempty"`
	// Unit 该条目的计量单位：空串 = 积分（credits，默认），`"token"` = Token 计数。
	//
	// 为什么需要它：有些渠道的额度**不以积分计量**（ZCode 的 billing/balance 是 token 桶，
	// MonkeyCode 的每日额度也是 token）。这类数字按「积分」展示会严重误导（上亿 token
	// 看起来像巨额积分），但完全隐藏又让用户看不到用量。故用 Unit 明确标注计量单位：
	// 前端据此把该渠道渲染成「Token 计数」面板，并与积分**分开统计**。
	// 零值（空串）保持既有语义 = 积分，向后兼容。
	Unit string `json:"unit,omitempty"`
}

// Summarize 按 Usable 标记汇总条目：返回 (可消耗剩余, 不可消耗剩余)。
// 供 app 层统一填充 ResourceDetail 接口的两个小计字段，避免多处各写一份循环。
func Summarize(items []ResourceItem) (usable, unusable int64) {
	for _, it := range items {
		if it.InfoOnly {
			continue
		}
		if it.Usable {
			usable += it.Remain
		} else {
			unusable += it.Remain
		}
	}
	return usable, unusable
}

// expireLoc 各渠道 ExpireAt 的统一墙钟口径（见各渠道 softRateResetLoc，固定 UTC+8）。
var expireLoc = time.FixedZone("UTC+8", 8*60*60)

// ExpiringWithin 统计 horizon 时长内到期的**可消耗**积分小计（临期额度）。
// 仅累计 Usable=true 且 ExpireAt 非空的条目——不可用池本工具消耗不到，
// 临期与否不影响路由决策，混入会虚高临期值。
// ExpireAt 是 YYYY-MM-DD 日期粒度（UTC+8 墙钟），精确到小时的 24h 判定无意义，
// 实际口径为「到期日 ≤ 明天」：今天到期/明天到期都算临期，后天起不算。
// ExpireAt 为空串（上游未下发，如 Qoder）或不可解析时不计入，调用方无需特判。
func ExpiringWithin(items []ResourceItem, horizon time.Duration) int64 {
	deadline := time.Now().In(expireLoc).Add(horizon)
	var expiring int64
	for _, it := range items {
		if !it.Usable || it.InfoOnly || it.ExpireAt == "" {
			continue
		}
		// 日期解析到当天零点（UTC+8），零点落在 deadline 之前即视为临期
		if t, err := time.ParseInLocation("2006-01-02", it.ExpireAt, expireLoc); err == nil && t.Before(deadline) {
			expiring += it.Remain
		}
	}
	return expiring
}
