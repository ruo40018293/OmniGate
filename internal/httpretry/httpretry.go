// Package httpretry 提供「传输层瞬态失败」的同号原地重试。
//
// # 为什么需要这一层（R92）
//
// 2026-11-03 23:21 与 23:23 两次对话在 traework 渠道「停下来」，日志是
// `dial tcp: lookup trae-api-cn.mchost.guru: no such host` —— 请求**根本没发出去**。
// 当时全仓只有 workbuddyai 一个渠道有 chat 重试，其余渠道 `Do(req)` 失败即返回，
// 而每个渠道只有 1 个账号（换号无处可换），于是 DNS 抖动一次就变成用户眼里的一次失败。
//
// 实测（`.tmp_speed.py` 逐条比对 app.log）：这类失败之后，下一次 chat 成功的时间
// 除一组渠道级故障外**全部在 1–16 秒内**，说明它们是秒级瞬态而非持续故障。
// 因此正确解法是「同一账号、同一请求、原地重试 2–3 次、指数退避」，
// 而不是把错误丢给用户，或去换一个并不存在的号。
//
// # 为什么是「克隆请求」而不是「调用方重建请求」
//
// 各渠道的 ChatStream 里，请求头（cosy 会话签名、ApplyHeaders 套件、idle 包装）
// 都在**发请求之前**现算。若把重试做成「调用方传一个 builder 闭包」，每个渠道都得
// 把建请求那段逻辑改成可按 attempt 参数化的函数，10 个渠道 × 各自的闭包变量，
// 极易改漏改错。而 `http.NewRequest` 对 `*bytes.Reader` / `*strings.Reader` /
// `*bytes.Buffer` 都会自动填 `GetBody`，因此重试只需从 `GetBody()` 取一份新 body、
// 克隆原请求（URL 与头原样保留）即可 —— 渠道代码只改一行 `Do` 调用。
//
// 不可重放（`GetBody` 为 nil 且 Body 非 nil）的请求**不重试**：宁可照旧一次失败，
// 也不发一个空 body 的上游请求（那会把「网络抖动」变成「上游 400」，更难排查）。
//
// # 边界（不许放宽）
//
//   - 只重试**传输层**错误（DNS、拨号超时、连接重置、响应头超时等）；
//     HTTP 4xx/5xx 不归本包管（渠道各自有业务语义，见 provider.ErrKind）。
//   - `context.Canceled` 不重试：那是客户端已断开或我们主动掐断，重试会白烧配额。
//   - 证书类错误（`x509` / `certificate` / `unsupported protocol` / `malformed`）不重试：
//     那是确定的配置问题，重试只会拖长故障时间。
//
// # cancel 所有权
//
// `wrap`（通常就是 `idle.WithCancel`）每次尝试都会被调用一次，产生**该次尝试独占**
// 的 ctx。失败的尝试由本包立即 cancel；成功那次把 cancel 原样返回给调用方，
// 由调用方交给 `idle.Monitor` 接管（Monitor 的 Close 会 cancel）。
//
// 这一点是硬要求：若跨多次尝试复用同一个 ctx，第一次失败时的 cancel 会连带取消
// 后续尝试的请求，重试变成必失败。
package httpretry

import (
	"context"
	"errors"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 生产参数：总尝试次数与退避基准。
//
// 3 次而非更多：DNS/连接类抖动实测在 1–16 秒内恢复，400ms+800ms≈1.2s 总等待
// 已能覆盖绝大多数瞬态；再往后加只会让用户多等而没有实质收益。
const (
	attempts  = 3
	baseDelay = 400 * time.Millisecond
)

// Doer 发一个请求，通常是某个 *http.Client 的 Do。
// 各渠道的 client 选择逻辑（StreamHTTP 优先、httpForStream() 等）留在渠道侧。
type Doer func(*http.Request) (*http.Response, error)

// Wrap 在每次尝试前加工克隆出来的请求，返回该次尝试独占的 cancel。
// 最典型的值就是 `idle.WithCancel`；nil 表示不做包装。
type Wrap func(*http.Request) (*http.Request, context.CancelFunc)

// OnRetry 在每次重试等待之前被调用（可为 nil），用于把重试写进日志/面板。
// attempt 是**失败的那一次**编号，wait 是即将等待的时长。
type OnRetry func(attempt int, wait time.Duration, err error)

// Do 发送请求，传输层瞬态错误时同号原地重试。
//
// req 必须是可重放的（`GetBody` 非 nil，或本来就没有 body）。
// 返回成功尝试的响应与该次尝试的 cancel（取消由调用方负责，见包注释）。
// 全部尝试失败时返回最后一次的错误（`errors.Is` 可穿透到原始错误）。
//
// 用于非流式请求时 wrap 传 nil 即可；流式请求必须传 idle.WithCancel，
// 否则成功后拿到的 cancel 会让 `idle.Monitor` 无法接管空闲监控。
func Do(req *http.Request, do Doer, wrap Wrap, onRetry OnRetry) (*http.Response, context.CancelFunc, error) {
	return doWith(req, do, wrap, onRetry, attempts, baseDelay, time.Sleep)
}

// doWith 是 Do 的可注入版本：max/base/sleep 由调用方给定，便于测试不真等。
func doWith(req *http.Request, do Doer, wrap Wrap, onRetry OnRetry, max int, base time.Duration, sleep func(time.Duration)) (*http.Response, context.CancelFunc, error) {
	// 重放能力判定一次即可：所有尝试共用同一个 base req。
	if req.Body != nil && req.GetBody == nil {
		max = 1
	}
	var lastErr error
	for attempt := 1; attempt <= max; attempt++ {
		// 第 1 次直接用原请求（Body 尚未被读）；后续每次克隆 + 取新 body。
		try := req
		if attempt > 1 {
			var cerr error
			try, cerr = cloneWithBody(req)
			if cerr != nil {
				// 取不到新 body 就无法重试，把最后一次错误原样交出去。
				break
			}
		}
		cancel := context.CancelFunc(func() {})
		if wrap != nil {
			try, cancel = wrap(try)
		}
		resp, err := do(try)
		if err == nil {
			return resp, cancel, nil
		}
		// 失败的尝试立刻释放 ctx，避免重试期间泄漏 goroutine / 悬挂连接。
		cancel()
		lastErr = err
		if !Retryable(err) || attempt == max {
			break
		}
		wait := backoff(attempt, base)
		if onRetry != nil {
			onRetry(attempt, wait, err)
		}
		sleep(wait)
	}
	return nil, noopCancel, lastErr
}

// noopCancel 失败时的占位 cancel。
//
// 各渠道的错误分支里都有一句 cancel()（成功时它来自 idle.WithCancel，必须由
// Monitor 接管）。失败时真正的 ctx 已在本包内被 cancel，若这里返回 nil，
// 那些分支会调用 nil func 直接 panic —— 故返回一个空操作，让渠道侧代码一字不改。
func noopCancel() {}

// cloneWithBody 克隆请求并换上新的 body reader。
//
// 无 body 的请求（GET 之类）根本不调用 GetBody —— 它的 GetBody 是 nil，
// 但请求本身完全可以重放，所以不能拿 GetBody 当唯一判据。
func cloneWithBody(req *http.Request) (*http.Request, error) {
	c := req.Clone(req.Context())
	if req.Body == nil {
		c.Body = nil
		return c, nil
	}
	if req.GetBody == nil {
		// 调用方已在 doWith 里把 max 降到 1，这里只是兜底，不静默发空 body。
		return nil, errors.New("request body is not replayable")
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	if body != nil {
		// Clone 已保留 ContentLength；这里只换 reader。
		c.Body = body
	}
	return c, nil
}

// Retryable 判断传输层错误是否值得同号重试。
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	// 客户端已断开 / 我方主动取消：重试没有意义，而且会白烧上游配额。
	if errors.Is(err, context.Canceled) {
		return false
	}
	// url.Error 自己也实现了 net.Error（Timeout/Temporary），若不先拆开，
	// 像 "invalid header field value" 这种**本地**错误也会被误判成瞬态。
	// 拆到底层错误再判：包装的是拨号错误就重试，包装的是本地错误就不重试。
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return Retryable(urlErr.Err)
	}
	s := strings.ToLower(err.Error())
	// 确定的配置/协议错误：重试只会拖长故障时间。
	for _, noRetry := range []string{"x509", "certificate", "unsupported protocol", "malformed"} {
		if strings.Contains(s, noRetry) {
			return false
		}
	}
	// net.Error 覆盖拨号超时、DNS 失败、连接重置等绝大多数瞬态错误。
	// context.DeadlineExceeded 也算（ResponseHeaderTimeout 与 DialContext 超时都走这里）。
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	for _, yes := range []string{
		"context deadline exceeded",
		"no such host",              // DNS 间歇性解析失败（R92 的直接触发因）
		"awaiting response headers", // Transport.ResponseHeaderTimeout
		"connection reset",
		"connection refused",
		"connection attempt failed",
		"broken pipe",
		"eof",
		"timeout",
		"empty reply",
		// Windows 特有的「对端异常」/ 路由不可达，均属瞬态。
		"wsarecv",
		"forcibly closed",
		"no route to host",
		"network is unreachable",
		"tls handshake timeout",
		"server misbehaving",
	} {
		if strings.Contains(s, yes) {
			return true
		}
	}
	return false
}

// backoff 第 attempt 次失败后的等待时长：指数退避 + ±40% 抖动。
//
// 抖动是为了避免多个渠道/请求在同一时刻对齐重试（对齐会一起再撞一次）。
func backoff(attempt int, base time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := base << (attempt - 1)
	if d <= 0 {
		return 0
	}
	jitter := time.Duration(rand.Int63n(int64(d/2+1))) - d/4
	return d + jitter
}

// LogRetry 返回一个把重试写进 app.log 的 OnRetry。
//
// 统一文案是为了后续能用一条 grep 统计「哪个渠道在抖动、重试几次救回来的」：
// `httpretry channel=<c> uid=<u> attempt=<n> 传输层瞬态错误，<wait> 后原地重试: <err>`
func LogRetry(channel, uid string) OnRetry {
	return func(attempt int, wait time.Duration, err error) {
		log.Printf("httpretry channel=%s uid=%s attempt=%d 传输层瞬态错误，%v 后原地重试: %s",
			channel, uid, attempt, wait.Round(time.Millisecond), oneLine(err))
	}
}

// oneLine 把错误压成一行（换行会让一条日志被拆成多条，破坏 grep 统计）。
func oneLine(err error) string {
	if err == nil {
		return ""
	}
	return strings.Join(strings.Fields(err.Error()), " ")
}
