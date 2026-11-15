package kilo

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"omnigate/internal/auth"
	"omnigate/internal/idle"
	"omnigate/internal/provider"
	"omnigate/internal/upstream"
)

// Client 实现 provider.Upstream。
//
// 这里没有账号状态、没有 token 池：免费池是**匿名**的，唯一「账号」是 main 注入的
// 虚拟账号（AnonymousAuth）。因此 RefreshToken 是空实现、额度接口不支持、无签到。
type Client struct {
	HTTP *http.Client
	// StreamHTTP 专供对话流：**不设 Client.Timeout**（R35）——
	// Client.Timeout 覆盖整个请求生命周期，对 SSE 意味着长回答必被从中间掐断。
	StreamHTTP *http.Client
	// IdleTimeout 流式空闲上限，0 = idle.DefaultTimeout。
	IdleTimeout time.Duration
	// Base 网关基址；测试注入 httptest 假上游用，生产不设。
	Base string
}

// New 构造生产 client。
func New() *Client { return NewWithBase(DefaultBase) }

// NewWithBase 构造指定基址的 client（测试/自建网关用）。
//
// 与其它渠道一致：禁 HTTP/2（上游网关对 h2 支持不稳），且流式 client 必须有
// ResponseHeaderTimeout 兜底——StreamHTTP 没有总超时，缺了它就等于上游不回
// 响应头时永久挂住（R91 raccoon 的教训）。
func NewWithBase(base string) *Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		TLSNextProto:          make(map[string]func(string, *tls.Conn) http.RoundTripper), // 强制 HTTP/1.1
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Client{
		HTTP:        &http.Client{Transport: tr, Timeout: requestTimeout},
		StreamHTTP:  &http.Client{Transport: tr.Clone()},
		IdleTimeout: idle.DefaultTimeout,
		Base:        strings.TrimRight(strings.TrimSpace(base), "/"),
	}
}

func (c *Client) base() string {
	if b := strings.TrimRight(strings.TrimSpace(c.Base), "/"); b != "" {
		return b
	}
	return DefaultBase
}

func (c *Client) idleTimeout() time.Duration {
	if c.IdleTimeout > 0 {
		return c.IdleTimeout
	}
	return idle.DefaultTimeout
}

func (c *Client) streamClient() *http.Client {
	if c.StreamHTTP != nil {
		return c.StreamHTTP
	}
	return c.HTTP
}

// AnonymousAuth 返回唯一虚拟账号。
//
// AccessToken 填 "public" 只是占位：auth 的若干路径（状态展示）要求非空，
// 而上游根本不需要 Authorization。ExpiresAt 必须是远期值（R22），否则
// NeedsRefresh 恒真，会天天去「刷新」一个没有刷新端点的账号。
func AnonymousAuth() *auth.Auth {
	return &auth.Auth{
		Kind:        Kind,
		AccessToken: "public",
		ExpiresAt:   noExpiry,
		UID:         AnonymousUID,
		Nickname:    AnonymousName,
	}
}

// headered 施加出站头。免费池不需要任何鉴权头，只要内容协商 + 显式 UA。
func headered(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
}

// ---------------------------------------------------------------------------
// 对话
// ---------------------------------------------------------------------------

// ChatStream 实现 provider.Upstream。
//
// 强制 `stream=true`：本渠道的 Stream/Aggregate 都按 SSE 帧解析，若让客户端拉成
// 非流式，行为会分成两条互不相同的路径，而只有流式那条被验证过。统一成流式。
func (c *Client) ChatStream(_ *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, http.StatusBadRequest, []byte(`{"error":{"message":"invalid JSON body"}}`), nil
	}
	obj["stream"] = true
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, http.StatusBadRequest, []byte(`{"error":{"message":"cannot encode body"}}`), nil
	}

	req, err := http.NewRequest(http.MethodPost, c.base()+EpChat, bytes.NewReader(out))
	if err != nil {
		return nil, 0, nil, err
	}
	headered(req)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.streamClient().Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, resp.StatusCode, raw, nil
	}
	return provider.NewIdleReader(resp.Body, c.idleTimeout()), resp.StatusCode, nil, nil
}

// ---------------------------------------------------------------------------
// 模型
// ---------------------------------------------------------------------------

// FetchModels 拉免费池清单。
//
// **失败与空清单都返回静态兜底且不报错**：这是刻意的。清单是「可选增强」，
// 拉不到时的正确行为是「少几个模型」而不是「整条渠道从面板消失」，
// 更不该被 handler 当成账号故障去冷却唯一账号。
func (c *Client) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) {
	rows, err := c.fetchListing()
	if err != nil || len(rows) == 0 {
		return staticFree(), nil
	}
	return filterFree(rows), nil
}

func (c *Client) fetchListing() ([]listingRow, error) {
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+EpModels, nil)
	if err != nil {
		return nil, err
	}
	headered(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("kilo models status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var wrap struct {
		Data []listingRow `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&wrap); err != nil {
		return nil, err
	}
	return wrap.Data, nil
}

// FetchModelPricing 免费池的价格恒为 0，且**是显式 0**（R96）：
// Explicit=true 让面板显示「免费」而不是「未知倍率」。
func (c *Client) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	models, err := c.FetchModels(a)
	if err != nil {
		return nil, err
	}
	yes := true
	out := make([]provider.ModelPricing, 0, len(models))
	for _, m := range models {
		out = append(out, provider.ModelPricing{
			Model:    m.ID,
			Channel:  Kind,
			Rate:     0,
			Explicit: &yes,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 账号面（免费池全部不支持）
// ---------------------------------------------------------------------------

// RefreshToken 空实现：免费池没有 token，也就没有可刷新的东西。
// 返回 nil 而不是报错——报错会让上层把账号判成 session dead。
func (c *Client) RefreshToken(*auth.Auth) error { return nil }

// UserResource 免费池没有积分概念，返回 (0, nil)。
func (c *Client) UserResource(*auth.Auth) (int64, error) { return 0, nil }

// UserResourceDetail 同上。**不能返回错误**：那会让面板每轮刷新都弹一条失败。
func (c *Client) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}

// DailyCheckin 明确报错而不是假装成功：本渠道没有签到活动，
// 返回 nil 会让调度器记一条假成功，用户以为每天有积分入账。
func (c *Client) DailyCheckin(*auth.Auth) error {
	return fmt.Errorf("kilo 免费池无签到活动")
}

// Classify 错误分类。
//
// 关键约束：**绝不返回 ErrSessionDead**。唯一账号被 Disable 后没有任何重登路径
// （免费池没有登录），整条渠道就此消失且无法人工恢复（R22/R23 的硬要求）。
// 因此这里把「凭据类」问题归到 ErrClient（可轮换但不罚号）而不是 session dead。
func (c *Client) Classify(status int, body string) provider.ErrKind {
	lower := strings.ToLower(body)
	switch {
	case status == http.StatusTooManyRequests:
		return provider.ErrSoftRate
	case status >= 500:
		return provider.ErrServer
	case status >= 400:
		for _, marker := range []string{"paid_model_auth_required", "moderation", "flagged", "filtered"} {
			if strings.Contains(lower, marker) {
				// 请求了付费模型 / 被内容策略拦：都是**这个请求**的问题，
				// 与渠道健康无关，归 ErrClient（不冷却）。
				return provider.ErrClient
			}
		}
		// 其余 4xx：不认识的业务错误，不罚号也不轮换（单账号渠道轮换必失败）。
		return provider.ErrPassthrough
	default:
		return provider.ErrNone
	}
}

// ---------------------------------------------------------------------------
// 流式
// ---------------------------------------------------------------------------

// Stream 逐帧透传上游 SSE，并把末帧 usage 回传给 handler 记账。
//
// 模型名改写走行内替换（见 modelRewriter）：客户端请求的是 `kilo/<id>`，
// 上游帧里回的是裸 `<id>`，不改写会让客户端看到自己的模型名被换掉。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	var usage map[string]any
	rr := newModelRewriter(r, model)
	err := upstream.StreamCapture(w, rr, func(u map[string]any) { usage = u })
	return usage, err
}

// Aggregate 非流式路径：读完整体后用同一套帧逻辑聚合成一个响应。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	rr := newModelRewriter(r, model)
	resp, err := upstream.Aggregate(rr)
	if err != nil {
		return nil, err
	}
	if model != "" {
		resp["model"] = model
	}
	return resp, nil
}

// modelRewriter 是按**行**替换流内模型名的 reader。
//
// 为什么按行：SSE 的帧边界是换行。若攒整条流再整体替换，流式就退化成一次性返回
// （客户端要等到结束才看到第一个字），这在本项目里是明确禁止的退化（R35/R56）。
//
// 替换规则：只改「值恰好等于上游裸 id」的那些帧。上游返回裸 id（如
// `kilo-auto/free`），客户端期望带渠道前缀（`kilo/kilo-auto/free`）；
// 已带前缀的帧原样保留——上游偶尔回显我们发过去的名字，再拼一次会变成
// `kilo/kilo/xxx`，客户端当场认不出自己的模型。值不等于裸 id 的帧也不动：
// 那不是我们请求的模型名，改它等于替上游编造数据。
type modelRewriter struct {
	src     *bufio.Reader
	pending []byte
	key     []byte // `"model":"`
	bare    []byte // 上游裸 id，例如 kilo-auto/free
	full    []byte // 客户端可见全名，例如 kilo/kilo-auto/free
}

func newModelRewriter(r io.Reader, model string) *modelRewriter {
	m := &modelRewriter{src: bufio.NewReaderSize(r, 64<<10)}
	full := strings.TrimSpace(model)
	if full == "" {
		return m
	}
	// 入参是客户端可见名（handler 传 clientModel），裸 id 要去掉渠道前缀。
	bare := strings.TrimPrefix(full, Kind+"/")
	if bare == "" {
		bare = full
	}
	m.key = []byte(`"model":"`)
	m.bare = []byte(bare)
	m.full = []byte(full)
	return m
}

func (m *modelRewriter) Read(p []byte) (int, error) {
	for {
		if len(m.pending) > 0 {
			n := copy(p, m.pending)
			m.pending = m.pending[n:]
			return n, nil
		}
		line, err := m.src.ReadBytes('\n')
		if len(line) > 0 {
			m.pending = m.rewrite(line)
			if len(m.pending) > 0 {
				continue
			}
		}
		if err != nil {
			return 0, err
		}
	}
}

// rewrite 在**一行内**逐个替换 `"model":"<裸id>"` 形态。
//
// 必须按「键 + 引号内值」解析而不是简单把 `"model":"` 整体替换：
// 后者会把原值留在后面，拼出 `"model":"kilo/x"x"` 这种坏帧。
func (m *modelRewriter) rewrite(line []byte) []byte {
	if len(m.key) == 0 {
		return line
	}
	out := make([]byte, 0, len(line)+16)
	rest := line
	for {
		idx := bytes.Index(rest, m.key)
		if idx < 0 {
			out = append(out, rest...)
			return out
		}
		valStart := idx + len(m.key)
		out = append(out, rest[:valStart]...)
		val := rest[valStart:]

		end := bytes.IndexByte(val, '"')
		if end < 0 {
			// 引号没闭合（半帧/截断）：原样交出去，交给截断检测处理。
			out = append(out, val...)
			return out
		}
		if bytes.Equal(val[:end], m.bare) {
			out = append(out, m.full...)
		} else {
			out = append(out, val[:end]...)
		}
		rest = val[end:]
	}
}
