package httpretry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fakeDoer 按预设的错误序列返回，用来断言「第几次尝试成功/失败」。
type fakeDoer struct {
	errs  []error // 第 i 次调用返回 errs[i]；用尽后返回 nil（成功）
	calls int32
}

func (d *fakeDoer) do(*http.Request) (*http.Response, error) {
	n := int(atomic.AddInt32(&d.calls, 1))
	select {
	case <-context.Background().Done(): // 永不触发，仅避免空 select 分支告警
	default:
	}
	if n <= len(d.errs) && d.errs[n-1] != nil {
		return nil, d.errs[n-1]
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(nil))}, nil
}

// countingWrap 模拟 idle.WithCancel：每次尝试产生独立的 ctx，并记录被创建的次数。
type countingWrap struct {
	wraps    int32
	cancels  []context.CancelFunc
	canceled []bool // 每次尝试的 cancel 是否被调用
}

func (w *countingWrap) wrap(req *http.Request) (*http.Request, context.CancelFunc) {
	idx := int(atomic.AddInt32(&w.wraps, 1)) - 1
	ctx, cancel := context.WithCancel(req.Context())
	var once syncOnce
	w.cancels = append(w.cancels, func() { once.Do(cancel) })
	for len(w.canceled) <= idx {
		w.canceled = append(w.canceled, false)
	}
	c := w.cancels[idx]
	wrapped := func() {
		w.canceled[idx] = true
		c()
	}
	return req.WithContext(ctx), wrapped
}

// syncOnce 极简 once（避免测试里再 import sync）。
type syncOnce struct{ done int32 }

func (o *syncOnce) Do(f func()) {
	if atomic.CompareAndSwapInt32(&o.done, 0, 1) {
		f()
	}
}

// newBodyPost 造一个**可重放**请求（http.NewRequest 会自动填 GetBody）。
func newBodyPost(t *testing.T, payload string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://example.invalid/x", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("造请求失败: %v", err)
	}
	if req.GetBody == nil {
		t.Fatalf("http.NewRequest 应自动填 GetBody（可重放前提）")
	}
	return req
}

// DNS 抖动是 R92 的直接触发因：必须在第 2 次成功，只发 1 条重试回调。
func TestDoRetriesTransientThenSucceeds(t *testing.T) {
	dnsErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("lookup trae-api-cn.mchost.guru: no such host")}
	d := &fakeDoer{errs: []error{dnsErr}}
	w := &countingWrap{}
	var retries int32
	resp, cancel, err := doWith(newBodyPost(t, "payload"), d.do, w.wrap,
		func(int, time.Duration, error) { atomic.AddInt32(&retries, 1) },
		3, time.Millisecond, func(time.Duration) {})
	if err != nil {
		t.Fatalf("第 2 次应成功，拿到 err=%v", err)
	}
	if resp == nil || resp.StatusCode != 200 {
		t.Fatalf("resp=%v", resp)
	}
	if cancel == nil {
		t.Fatalf("成功分支必须把 cancel 交回调用方（idle.Monitor 要用）")
	}
	cancel() // 显式释放，避免测试泄漏
	if got := atomic.LoadInt32(&d.calls); got != 2 {
		t.Fatalf("期望 2 次尝试，实际 %d", got)
	}
	if got := atomic.LoadInt32(&w.wraps); got != 2 {
		t.Fatalf("每次尝试必须独占一个 ctx，期望 wrap 2 次，实际 %d", got)
	}
	if got := atomic.LoadInt32(&retries); got != 1 {
		t.Fatalf("期望 1 条重试回调，实际 %d", got)
	}
	// 第一次尝试失败后，它的 ctx 必须被 cancel（否则泄漏 goroutine/连接）。
	if len(w.canceled) < 2 || !w.canceled[0] {
		t.Fatalf("失败尝试的 cancel 必须被调用：%v", w.canceled)
	}
}

// 每次重试都必须带**完整的** body：复用被读过的 body 会让上游收到空请求。
func TestRetryReplaysBody(t *testing.T) {
	var bodies []string
	d := &fakeDoer{errs: []error{errors.New("connection reset by peer")}}
	do := func(req *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(req.Body)
		bodies = append(bodies, string(b))
		return d.do(req)
	}
	_, cancel, err := doWith(newBodyPost(t, "hello-payload"), do, nil, nil, 3, time.Millisecond, func(time.Duration) {})
	if err != nil {
		t.Fatalf("第 2 次应成功: %v", err)
	}
	if cancel != nil {
		cancel()
	}
	if len(bodies) != 2 {
		t.Fatalf("期望 2 次尝试，实际 %d", len(bodies))
	}
	for i, b := range bodies {
		if b != "hello-payload" {
			t.Fatalf("第 %d 次尝试的 body 不完整: %q", i+1, b)
		}
	}
}

// 不可重放（GetBody 为 nil 且 Body 非 nil）时必须只发一次，不能发空 body。
func TestUnreplayableRequestIsSingleAttempt(t *testing.T) {
	d := &fakeDoer{errs: []error{errors.New("no such host")}}
	req, err := http.NewRequest(http.MethodPost, "http://example.invalid/x", nil)
	if err != nil {
		t.Fatalf("%v", err)
	}
	// 手工塞一个不可重放的 body。
	req.Body = io.NopCloser(strings.NewReader("once"))
	req.GetBody = nil
	_, _, err = doWith(req, d.do, nil, nil, 3, time.Millisecond, func(time.Duration) {})
	if err == nil || !strings.Contains(err.Error(), "no such host") {
		t.Fatalf("期望返回原错误，实际 %v", err)
	}
	if got := atomic.LoadInt32(&d.calls); got != 1 {
		t.Fatalf("不可重放的请求只能发 1 次，实际 %d", got)
	}
}

// 不可重试错误（证书类）必须一次就交出去：重试只会拖长故障时间。
func TestDoDoesNotRetryCertError(t *testing.T) {
	d := &fakeDoer{errs: []error{errors.New("x509: certificate signed by unknown authority")}}
	_, _, err := doWith(newBodyPost(t, "payload"), d.do, nil, nil, 3, time.Millisecond, func(time.Duration) {})
	if err == nil || !strings.Contains(err.Error(), "x509") {
		t.Fatalf("期望原样返回证书错误，实际 %v", err)
	}
	if got := atomic.LoadInt32(&d.calls); got != 1 {
		t.Fatalf("证书错误不该重试，实际尝试 %d 次", got)
	}
}

// 客户端已经断开（context.Canceled）时重试会白烧上游配额。
func TestDoDoesNotRetryContextCanceled(t *testing.T) {
	canceled := fmt.Errorf("post: %w", context.Canceled)
	d := &fakeDoer{errs: []error{canceled}}
	_, _, err := doWith(newBodyPost(t, "payload"), d.do, nil, nil, 3, time.Millisecond, func(time.Duration) {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("期望保留 context.Canceled 可穿透，实际 %v", err)
	}
	if got := atomic.LoadInt32(&d.calls); got != 1 {
		t.Fatalf("context.Canceled 不该重试，实际 %d 次", got)
	}
}

// 重试耗尽后必须把**最后一次**的原始错误交出去（不是包装成新错误）。
func TestDoExhaustedKeepsLastError(t *testing.T) {
	e1 := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	e2 := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("lookup x: no such host")}
	d := &fakeDoer{errs: []error{e1, e2, errors.New("connection reset by peer")}}
	_, _, err := doWith(newBodyPost(t, "payload"), d.do, nil, nil, 3, time.Millisecond, func(time.Duration) {})
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("期望返回最后一次错误，实际 %v", err)
	}
	if got := atomic.LoadInt32(&d.calls); got != 3 {
		t.Fatalf("耗尽时应有 3 次尝试，实际 %d", got)
	}
}

// wrap 自身返回错误（如 idle.WithCancel 之后的自定义加工失败）必须立刻交出去。
// 退避必须指数增长且有抖动上界。
func TestBackoffGrowsAndJitters(t *testing.T) {
	base := 100 * time.Millisecond
	for attempt, want := range map[int]time.Duration{1: base, 2: 2 * base, 3: 4 * base} {
		for i := 0; i < 50; i++ {
			d := backoff(attempt, base)
			// 抖动 ±40%：落在 [0.6, 1.4] 倍区间内。
			if d < want*6/10 || d > want*14/10 {
				t.Fatalf("attempt=%d 期望落在 [%v,%v]，实际 %v", attempt, want*6/10, want*14/10, d)
			}
		}
	}
}

// 判据必须覆盖生产里真实出现过的每一类错误（含 Windows 的 wsarecv）。
func TestRetryableCategories(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"dns", errors.New(`dial tcp: lookup trae-api-cn.mchost.guru: no such host`), true},
		{"rh-timeout", errors.New("net/http: timeout awaiting response headers"), true},
		{"dial-timeout", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("i/o timeout")}, true},
		{"conn-reset", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, true},
		{"refused", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, true},
		{"wsarecv", errors.New("wsarecv: An existing connection was forcibly closed by the remote host."), true},
		{"broken-pipe", errors.New("write tcp: broken pipe"), true},
		{"eof", io.ErrUnexpectedEOF, true},
		{"deadline", context.DeadlineExceeded, true},
		{"x509", errors.New("x509: certificate expired"), false},
		{"unsupported-protocol", errors.New(`http2: unsupported protocol scheme ""`), false},
		{"malformed", errors.New("malformed HTTP response"), false},
		{"canceled", context.Canceled, false},
		{"local-header-error", &url.Error{Op: "Post", URL: "x", Err: errors.New("invalid header field value")}, false},
	}
	for _, c := range cases {
		if got := Retryable(c.err); got != c.want {
			t.Errorf("%s: Retryable()=%v，期望 %v", c.name, got, c.want)
		}
	}
}

// 日志必须一行：多行日志会让 grep 统计失真。
func TestLogRetryIsOneLine(t *testing.T) {
	var got string
	onRetry := func(_ int, _ time.Duration, err error) { got = oneLine(err) }
	onRetry(1, time.Millisecond, errors.New("dial tcp:\n  lookup x: no such host"))
	if strings.Contains(got, "\n") {
		t.Fatalf("日志被换行拆散: %q", got)
	}
	if !strings.Contains(got, "no such host") {
		t.Fatalf("日志丢了原始信息: %q", got)
	}
}
