// client.go QoderCN 上游客户端：业务 API（dt- Bearer）+
// COSY 签名对话转发 + 错误分类，实现 provider.Upstream 接口。
// 代码级复制自 internal/qoder/client.go 后按 qoder2api 参数改造：
//   - 登录/刷新后从 /api/v1/userinfo 实测 userType（identity 签名用实测值）
//   - 模型路由仅用「上次成功拉取的动态表」缓存（无静态兜底）
//   - 签到：双路径（daily-check-in + campaigns，实测主路径是 campaigns）
package qodercn

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"omnigate/internal/auth"
	"omnigate/internal/capture"
	"omnigate/internal/idle"
	"omnigate/internal/provider"
)

// Client QoderCN 上游客户端。
type Client struct {
	HTTP    *http.Client
	Base    string // 业务 API，默认 https://openapi.qoder.com.cn
	Gateway string // 推理网关，默认 https://gateway.qoder.com.cn
	// StreamHTTP 用于对话流（SSE）：不设整体 Timeout，避免长回答在超时点被掐断
	// 且无终止帧（R35 / issue #42）。
	StreamHTTP *http.Client
	// IdleTimeout 聊天 SSE 流中空闲超时（StreamHTTP 无总时长上限，靠本字段兜底
	// 「上游中途卡死」）。<=0 表示禁用（测试用）。
	IdleTimeout time.Duration
	// FirstByteTimeout 首帧看门狗：响应头已到但 body 迟迟不给数据的判定阈值。
	// 上游排队时把 body 挂住（实测 180s 才吐 10605 排队帧），官方客户端此时显示
	// 「排队中/预计等待 N 秒」；网关不能陪等，超阈值即按排队处理并熔断该模型。
	// <=0 表示禁用（测试用）。见 queue.go 与 AGENTS.md R46。
	FirstByteTimeout time.Duration

	// modelMap 客户端名（display_name 规范化）→ 上游 model key；
	// cache 为最近一次成功拉取的完整模型表（无静态兜底，仅此缓存）。
	modelMu  sync.RWMutex
	modelMap map[string]string
	cache    []ModelEntry

	// utMu 保护 uid→userType 实测缓存（登录后/首次请求时填充）。
	utMu      sync.RWMutex
	userTypes map[string]string

	// queueMu 保护「排队熔断」窗口：观察到上游 10605 排队后，在该模型上短时快速失败，
	// 避免每个请求都陪上游挂 180 秒（详见 queue.go 与 AGENTS.md R46）。
	queueMu    sync.Mutex
	queueUntil map[string]time.Time
	queueInfo  map[string]*QueueError
}

// defaultFirstByteTimeout 首帧看门狗默认阈值。
//
// 取值依据（2026-10-21 实测）：本渠道正常请求 firstTokenDuration≈338ms，
// 而排队时上游把 body 挂住整整 180 秒才吐 10605。30 秒既远大于正常首帧
// （含长思考模型的首帧），又能在排队时把「等 180 秒」压到「等 30 秒后 429」。
const defaultFirstByteTimeout = 30 * time.Second

// New 生产默认。Qoder gateway 对 HTTP/2 不友好（stream INTERNAL_ERROR），强制 HTTP/1.1。
func New() *Client {
	return NewWithTimeout(180 * time.Second)
}

// NewWithTimeout 指定上游 HTTP 超时；配置连接池。
func NewWithTimeout(timeout time.Duration) *Client {
	tr := &http.Transport{
		// 自动读取环境变量中的代理配置（HTTP_PROXY / HTTPS_PROXY / NO_PROXY）。
		// 不设时 Go 默认行为就是 ProxyFromEnvironment，但显式 Transport 会覆盖默认值，
		// 故必须在这里补上——否则自建 Transport 的渠道永远不走代理。
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{}, // 强制 HTTP/1.1
	}
	return &Client{
		HTTP:             &http.Client{Timeout: timeout, Transport: tr},
		StreamHTTP:       &http.Client{Transport: tr}, // 共用 Transport，不设 Timeout
		IdleTimeout:      idle.DefaultTimeout,         // 流中空闲监控兜底（见 internal/idle）
		FirstByteTimeout: defaultFirstByteTimeout,     // 首帧看门狗（排队识别，R46）
		Base:             OpenAPIBase,
		Gateway:          GatewayBase,
		modelMap:         map[string]string{},
		userTypes:        map[string]string{},
	}
}

// NewWithBase 测试用：覆盖 base/gateway。
func NewWithBase(base, gateway string) *Client {
	c := New()
	c.Base = base
	c.Gateway = gateway
	return c
}

// setModelMap 记录动态模型映射（客户端名 → key）。
func (c *Client) setModelMap(m map[string]string) {
	c.modelMu.Lock()
	c.modelMap = m
	c.modelMu.Unlock()
}

// modelKey 客户端模型名 → 上游 model key：动态映射优先；
// 无静态兜底表，未命中时原样返回（上游对未知 key 走兜底，不计 quota —— qoder2api 注释）。
func (c *Client) modelKey(clientName string) string {
	c.modelMu.RLock()
	defer c.modelMu.RUnlock()
	if k := c.modelMap[clientName]; k != "" {
		return k
	}
	return clientName
}

// userTypeOf 返回 uid 的实测 userType；无缓存返回缺省 personal_standard。
func (c *Client) userTypeOf(a *auth.Auth) string {
	c.utMu.RLock()
	defer c.utMu.RUnlock()
	if ut := c.userTypes[a.UID]; ut != "" {
		return ut
	}
	return "personal_standard"
}

// setUserType 记录实测 userType。
func (c *Client) setUserType(uid, ut string) {
	if uid == "" || ut == "" {
		return
	}
	c.utMu.Lock()
	c.userTypes[uid] = ut
	c.utMu.Unlock()
}

// FetchUserInfo 拉取 /api/v1/userinfo 并缓存 userType。
// 返回 (name, userType, error)；name 为用户显示名（邮箱或昵称），
// 供登录流程回填 auth.Nickname（设备流响应里无 nickname）。
func (c *Client) FetchUserInfo(a *auth.Auth) (string, string, error) {
	dt := a.JWT()
	if dt == "" {
		return "", "", fmt.Errorf("no dt- available")
	}
	req, err := http.NewRequest(http.MethodGet, c.Base+EpUserInfo, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+dt)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("userinfo http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var info struct {
		UserType string `json:"userType"`
		Name     string `json:"name"`
		Email    string `json:"email"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return "", "", fmt.Errorf("userinfo parse: %w", err)
	}
	c.setUserType(a.UID, info.UserType)
	name := info.Name
	if name == "" {
		name = info.Email // 部分账号 name 为空，email 兑底
	}
	log.Printf("qodercn userinfo uid=%s userType=%s name=%s", a.UID, info.UserType, name)
	return name, info.UserType, nil
}

func billingHeaders(req *http.Request, dt string) {
	req.Header.Set("Authorization", "Bearer "+dt)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
}

func (c *Client) get(path, dt string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, c.Base+path, nil)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, dt)
	return c.HTTP.Do(req)
}

func (c *Client) post(path, dt string, body []byte) (*http.Response, error) {
	if body == nil {
		body = []byte("{}")
	}
	req, err := http.NewRequest(http.MethodPost, c.Base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	billingHeaders(req, dt)
	return c.HTTP.Do(req)
}

func readBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// ---------------------------------------------------------------------------
// provider.Upstream 实现
// ---------------------------------------------------------------------------

// RefreshToken 用 drt- 走 /api/v1/deviceToken/refresh 轮换 dt/drt。
// 401/403 → ErrSessionDead（需重新 OAuth 登录）。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	log.Printf("qodercn refresh start uid=%s", a.UID)
	if strings.TrimSpace(a.RefreshToken) == "" {
		err := fmt.Errorf("no drt- available")
		log.Printf("qodercn refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	body, _ := json.Marshal(map[string]string{"refresh_token": a.RefreshToken})
	req, err := http.NewRequest(http.MethodPost, c.Base+EpDTRefresh, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// refresh 用独立短超时客户端
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("qodercn refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// drt 失效 → 需重新登录
		err := &provider.Error{Kind: provider.ErrSessionDead, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
		log.Printf("qodercn refresh session-dead uid=%s err=%v", a.UID, err)
		return err
	}
	if resp.StatusCode >= 400 {
		err := fmt.Errorf("deviceToken refresh http %d: %s", resp.StatusCode, truncate(string(raw), 200))
		log.Printf("qodercn refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	var out struct {
		Token        string `json:"token"`
		DeviceToken  string `json:"device_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    string `json:"expires_at"`
		ExpiresIn    int64  `json:"expires_in"` // ms
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		log.Printf("qodercn refresh parse failed uid=%s err=%v", a.UID, err)
		return fmt.Errorf("deviceToken refresh parse: %w", err)
	}
	dt := out.Token
	if dt == "" {
		dt = out.DeviceToken
	}
	if dt == "" || out.RefreshToken == "" {
		return fmt.Errorf("deviceToken refresh: incomplete token pair")
	}
	a.AccessToken = dt
	a.RefreshToken = out.RefreshToken
	now := time.Now()
	if out.ExpiresIn > 0 {
		a.ExpiresAt = now.Add(time.Duration(out.ExpiresIn) * time.Millisecond).Unix()
	} else if out.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, out.ExpiresAt); err == nil {
			a.ExpiresAt = t.Unix()
		}
	}
	if a.ExpiresAt == 0 {
		a.ExpiresAt = now.Add(30 * 24 * time.Hour).Unix() // dt 观测寿命 30d
	}
	log.Printf("qodercn refresh success uid=%s expires_at=%d", a.UID, a.ExpiresAt)
	return nil
}

// streamHTTP 返回对话流专用 client（无整体 Timeout）；未配置时回退到 HTTP。
func (c *Client) streamHTTP() *http.Client {
	if c.StreamHTTP != nil {
		return c.StreamHTTP
	}
	return c.HTTP
}

// ChatStream 发 chat 请求并返回原始嵌套 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc 为 nil、respBody 为上游响应体、err 为 nil；只有传输层失败才返回 err。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	// body 是 server 侧改写后的 OpenAI 请求；qoder 需要 messages/model/tools/max_tokens。
	var reqOpenAI struct {
		Model           string           `json:"model"`
		Messages        []map[string]any `json:"messages"`
		Tools           []any            `json:"tools"`
		ReasoningEffort string           `json:"reasoning_effort"`
		MaxTokens       int              `json:"max_tokens"`
		Thinking        *struct {
			Type string `json:"type"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(body, &reqOpenAI); err != nil {
		return nil, 0, nil, fmt.Errorf("parse chat body: %w", err)
	}
	clientName := reqOpenAI.Model
	modelKey := c.modelKey(clientName)
	if modelKey == "" {
		modelKey = clientName
	}

	// 排队熔断命中：直接快速失败，不陪上游挂 180 秒。
	// retry_after 给「剩余秒数」而非固定值，客户端退避才准。
	if q, remain := c.queueBlocked(clientName); q != nil {
		retryAfter := int(remain.Seconds() + 0.5)
		log.Printf("qodercn chat_stream uid=%s model=%s: 排队熔断中，快速返回 429（%s，剩余 %ds）",
			a.UID, clientName, q.Error(), retryAfter)
		return nil, http.StatusTooManyRequests, queuedBody(clientName, q, retryAfter), nil
	}

	// 思考开关：reasoning_effort 或 thinking:{type:"enabled"} → 启用推理
	enableReasoning := false
	if reqOpenAI.ReasoningEffort != "" {
		enableReasoning = true
	} else if reqOpenAI.Thinking != nil && reqOpenAI.Thinking.Type == "enabled" {
		enableReasoning = true
	}

	// 上下文档位（issue #27）：客户端 context_length/context_window 提示 → 模型默认档
	mc := c.modelEntry(modelKey)
	contextWindow := resolveContextWindow(parseContextWindowHint(body), mc)

	rawBody, err := buildAgentBody(reqOpenAI.Messages, mc, reqOpenAI.Tools, enableReasoning, reqOpenAI.MaxTokens, c.userTypeOf(a), contextWindow)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("build qodercn body: %w", err)
	}
	encoded := qoderEncode(rawBody)
	url := c.Gateway + EpChat
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(encoded))
	if err != nil {
		return nil, 0, nil, err
	}
	dt := a.JWT()
	sess, err := NewCosySession(a.MachineID, a.MachineToken, a.MachineType, a.Nickname, a.UID, dt, a.RefreshToken, c.userTypeOf(a))
	if err != nil {
		return nil, 0, nil, fmt.Errorf("cosy session: %w", err)
	}
	if err := sess.ApplyHeaders(req, encoded, url, a.UID, "text/event-stream", true, modelKey); err != nil {
		return nil, 0, nil, fmt.Errorf("cosy headers: %w", err)
	}
	req, cancel := idle.WithCancel(req)
	resp, err := c.streamHTTP().Do(req)
	if err != nil {
		cancel()
		log.Printf("qodercn chat_stream uid=%s model=%s: transport error: %v", a.UID, modelKey, err)
		// R54 异常取证：传输层失败也留证（截断/握手失败常是风控前置信号）。
		capture.Save(capture.Record{
			Channel: "qodercn",
			UID:     a.UID,
			Model:   modelKey,
			Err:     err.Error(),
			Request: []byte(encoded),
		})
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		cancel()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		log.Printf("qodercn chat_stream uid=%s model=%s: upstream %d body=%s",
			a.UID, modelKey, resp.StatusCode, truncate(string(raw), 200))
		// R54 异常取证：真实失败时的「出站字节 + 上游回包」是唯一能定位的依据。
		capture.Save(capture.Record{
			Channel:  "qodercn",
			UID:      a.UID,
			Model:    modelKey,
			Status:   resp.StatusCode,
			Classify: fmt.Sprint(Classify(resp.StatusCode, string(raw))),
			Request:  []byte(encoded),
			Response: raw,
		})
		// HTTP 级排队快速拒绝（R46 第三路径）：同样写熔断窗口，后续重试毫秒级失败。
		if q := parseQueueBody(raw); q != nil {
			c.rememberQueue(clientName, q)
		}
		return nil, resp.StatusCode, raw, nil
	}
	// 首帧看门狗（R46）：上游排队时响应头立刻返回、body 挂住（实测 180s 后才吐
	// 10605 排队帧），官方客户端此时显示「排队中·预计等待 N 秒」。网关不能陪等——
	// 在首个字节到达前设阈值，超时即按排队快速失败并短时熔断该模型。
	// 注意只在「首字节之前」计时：首帧到达后面交 idle.Monitor 管空闲（R35/#42）。
	br := bufio.NewReaderSize(resp.Body, 64*1024)
	if c.FirstByteTimeout > 0 {
		peeked := make(chan error, 1)
		go func() {
			_, perr := br.Peek(1) // 阻塞到有任何字节或错误
			peeked <- perr
		}()
		timer := time.NewTimer(c.FirstByteTimeout)
		defer timer.Stop()
		select {
		case perr := <-peeked:
			if perr != nil {
				cancel()
				resp.Body.Close()
				log.Printf("qodercn chat_stream uid=%s model=%s: 上游无首帧即结束: %v", a.UID, modelKey, perr)
				return nil, http.StatusBadGateway, []byte(`{"error":{"message":"upstream closed before first frame","type":"upstream_error","code":"upstream_empty"}}`), nil
			}
		case <-timer.C:
			cancel()
			resp.Body.Close()
			q := &QueueError{ModelKey: modelKey, Hang: true}
			c.rememberQueueFor(clientName, q, queueHangWindow)
			retryAfter := int(queueHangWindow.Seconds())
			log.Printf("qodercn chat_stream uid=%s model=%s: 首帧看门狗超时 %v，按排队处理并熔断 %v",
				a.UID, clientName, c.FirstByteTimeout, queueHangWindow)
			return nil, http.StatusTooManyRequests, queuedBody(clientName, q, retryAfter), nil
		}
	}
	// 成功分支：cancel 所有权交给 idle.Monitor（其 Close 会 cancel；静默超时也会 cancel）。
	// 用 br 包一层，保证已被 Peek 进缓冲的首帧不会丢。
	return idle.Monitor(&bufferedBody{Reader: br, body: resp.Body}, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
}

// bufferedBody 把 bufio.Reader（含预读缓冲）与底层 body 合成一个 ReadCloser，
// Close 只关底层一次。
type bufferedBody struct {
	Reader *bufio.Reader
	body   io.ReadCloser
}

func (b *bufferedBody) Read(p []byte) (int, error) { return b.Reader.Read(p) }
func (b *bufferedBody) Close() error               { return b.body.Close() }

// modelEntry 按 key 找缓存中的模型条目（构造 model_config 全字段用）；找不到返回 nil（auto 兜底）。
func (c *Client) modelEntry(key string) *ModelEntry {
	for i := range c.cache {
		if c.cache[i].Key == key {
			return &c.cache[i]
		}
	}
	return nil
}

// UserResource 查询账号当前可花费积分（基础 + 赠送聚合）。
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	dt := a.JWT()
	if dt == "" {
		return 0, fmt.Errorf("no dt- available")
	}
	resp, err := c.get(EpQuotaUsage, dt)
	if err != nil {
		return 0, err
	}
	raw, err := readBody(resp)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode >= 400 {
		return 0, c.classifyError(resp.StatusCode, string(raw))
	}
	var q struct {
		UserQuota struct {
			Remaining float64 `json:"remaining"`
		} `json:"userQuota"`
		AddOnQuota struct {
			Remaining float64 `json:"remaining"`
		} `json:"addOnQuota"`
		IsQuotaExceeded bool `json:"isQuotaExceeded"`
	}
	if err := json.Unmarshal(raw, &q); err != nil {
		return 0, fmt.Errorf("quota parse: %w", err)
	}
	return int64(q.UserQuota.Remaining + q.AddOnQuota.Remaining), nil
}

// UserResourceDetail 查询积分明细：userQuota + addOnQuota 两个条目。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	dt := a.JWT()
	if dt == "" {
		return 0, nil, fmt.Errorf("no dt- available")
	}
	resp, err := c.get(EpQuotaUsage, dt)
	if err != nil {
		return 0, nil, err
	}
	raw, err := readBody(resp)
	if err != nil {
		return 0, nil, err
	}
	if resp.StatusCode >= 400 {
		return 0, nil, c.classifyError(resp.StatusCode, string(raw))
	}
	var q struct {
		UserQuota struct {
			Total     float64 `json:"total"`
			Used      float64 `json:"used"`
			Remaining float64 `json:"remaining"`
		} `json:"userQuota"`
		AddOnQuota struct {
			Total     float64 `json:"total"`
			Used      float64 `json:"used"`
			Remaining float64 `json:"remaining"`
		} `json:"addOnQuota"`
	}
	if err := json.Unmarshal(raw, &q); err != nil {
		return 0, nil, fmt.Errorf("quota parse: %w", err)
	}
	total := int64(q.UserQuota.Remaining + q.AddOnQuota.Remaining)
	items := []provider.ResourceItem{
		{Name: "用户套餐", Total: int64(q.UserQuota.Total), Used: int64(q.UserQuota.Used), Remain: int64(q.UserQuota.Remaining), Key: "userQuota", Usable: true},
	}
	if q.AddOnQuota.Total > 0 || q.AddOnQuota.Remaining > 0 {
		items = append(items, provider.ResourceItem{Name: "赠送额度", Total: int64(q.AddOnQuota.Total), Used: int64(q.AddOnQuota.Used), Remain: int64(q.AddOnQuota.Remaining), Key: "addOnQuota", Usable: true})
	}
	return total, items, nil
}

// DailyCheckin 每日签到（campaigns 活动路径）。详见 checkin.go。
// 已签到/无活动均视为成功（幂等），返回 nil；仅网络/401 等才返回 error。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	_, err := checkin(a)
	return err
}

// DailyCheckinReport 实现 provider.CheckinReporter：额外返回结构化状态，
// 供调度器区分 no_campaign（需在窗口内重试）与已领取（当日完成）。
func (c *Client) DailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {
	return checkin(a)
}

// Classify 实现 provider.Upstream。
func (c *Client) Classify(status int, body string) provider.ErrKind { return Classify(status, body) }

// Stream 实现 provider.Upstream（嵌套 SSE → 标准 OpenAI SSE 透传）。
// model 为客户端请求的模型名，直接注入每个 chunk（上游恒为 "auto"）。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	usage, err := StreamCapture(w, r, model, nil)
	// 流内观察到排队：记下熔断窗口，后续请求快速失败（客户端可立即换模型）。
	var q *QueueError
	if errors.As(err, &q) {
		c.rememberQueue(model, q)
	}
	return usage, err
}

// Aggregate 实现 provider.Upstream（嵌套 SSE 聚合）。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	return aggregate(r, model)
}

// ---------------------------------------------------------------------------
// 错误分类
// ---------------------------------------------------------------------------

var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit", "isquotaexceeded\":true",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
func Classify(status int, body string) provider.ErrKind {
	if status == http.StatusPaymentRequired {
		return provider.ErrHardCredit
	}
	lower := strings.ToLower(body)
	// TOKEN_EXPIRE 优先于通用 401
	if status == http.StatusUnauthorized && strings.Contains(body, "TOKEN_EXPIRE") {
		return provider.ErrSessionDead
	}
	if status == http.StatusUnauthorized {
		return provider.ErrSessionDead
	}
	// 排队（10605 / isQueued）：模型侧容量问题，请求级透传，不得冷却账号。
	// 必须排在通用 429 分支**之前**（否则会被归为 soft_rate → 账号冷却）。
	if strings.Contains(lower, "model_queued") || strings.Contains(lower, "isqueued") ||
		strings.Contains(body, "10605") {
		return provider.ErrPassthrough
	}
	// 429 优先于 hardRule：限流 body 高频带 "quota exceeded"。
	if status == http.StatusTooManyRequests {
		return provider.ErrSoftRate
	}
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return provider.ErrHardCredit
		}
	}
	// 非 429 但 body 含限流文案 → 软限流
	if strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "usage limit") || strings.Contains(lower, "请求过于频繁") {
		return provider.ErrSoftRate
	}
	if status == http.StatusNotFound {
		return provider.ErrNotFound
	}
	if status >= 500 {
		return provider.ErrServer
	}
	if status >= 400 {
		if strings.Contains(lower, "blocked by security policy") ||
			strings.Contains(lower, "unapproved channel") ||
			strings.Contains(lower, "illegal api invocation") {
			return provider.ErrContentBlocked
		}
		if (status == 400 || status == 404) &&
			(strings.Contains(lower, "prompt is too long") || strings.Contains(lower, "11115")) {
			return provider.ErrPromptTooLong
		}
		if status == 403 && strings.TrimSpace(body) == "" {
			return provider.ErrWafBlock
		}
		if strings.Contains(lower, "request illegal") || strings.Contains(lower, "trial not activated") {
			return provider.ErrAccountFault
		}
		return provider.ErrClient
	}
	return provider.ErrNone
}

// classifyError 转成 provider.Error。
func (c *Client) classifyError(status int, body string) error {
	return &provider.Error{Kind: Classify(status, body), Status: status, Msg: truncate(body, 200)}
}

// EnsureFingerprint 为账号生成/保持 COSY 机器指纹（MachineID/Token/Type）。
// 幂等：已有则保留。与 qoder2api 的稳定派生不同，沿用随机生成 + 持久化
// （凭据文件即存储，跨重启稳定）。
func EnsureFingerprint(a *auth.Auth) {
	if a.MachineID == "" {
		a.MachineID = uuid4()
	}
	if a.MachineToken == "" {
		seed := []byte(uuid4() + uuid4())
		if len(seed) > 50 {
			seed = seed[:50]
		}
		a.MachineToken = base64.RawURLEncoding.EncodeToString(seed)
	}
	if a.MachineType == "" {
		a.MachineType = strings.ReplaceAll(uuid4(), "-", "")[:18]
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
