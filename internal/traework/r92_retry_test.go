package traework

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"omnigate/internal/auth"
	"omnigate/internal/httpretry"
	"omnigate/internal/idle"
)

// chatBody 一个最小可用的 chat 请求体（PrepareBody 只做脱敏与字段搬运）。
const chatBody = `{"model":"trae/m","messages":[{"role":"user","content":"hi"}],"stream":true}`

// 视察点：R92 只在**传输层**错误上重试。DNS 抖动（no such host）与连接被掐断
// （EOF/连接重置）属于同一类，1–16 秒内自愈；而证书错误、400 之类重试无意义。

// TestChatStreamRetriesTransientThenSucceeds 第 1 次连接被掐断、第 2 次正常 → 用户不该看到失败。
//
// 这是 2026-11-03 23:21/23:23 两次「任务停下来」的直接回归：当时上游 DNS 抖了一下，
// 我们一次就放弃了；上游方 4 秒后就恢复了，但客户端已经收到错误。
func TestChatStreamRetriesTransientThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpChat {
			http.NotFound(w, r)
			return
		}
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			// 不写任何响应就断开连接 = 客户端读到 EOF，属可重试的传输层错误。
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
					return
				}
			}
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := New()
	c.AgentHost = srv.URL
	c.HTTP = srv.Client()
	c.StreamHTTP = srv.Client()

	rc, status, _, err := c.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(chatBody))
	if err != nil {
		t.Fatalf("传输层抖动一次就被放弃了：%v", err)
	}
	if rc != nil {
		_ = rc.Close()
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d want 200", status)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("上游请求数=%d want 2（第一次失败必须同号原地重试）", got)
	}
}

// TestChatStreamDoesNotRetryCertError 证书错误重试也不会好，必须一次就放弃。
func TestChatStreamDoesNotRetryCertError(t *testing.T) {
	certErr := errors.New("x509: certificate signed by unknown authority")
	tr := &recordingTripper{err: certErr}
	c := New()
	c.AgentHost = "https://example.invalid"
	c.HTTP = &http.Client{Transport: tr}
	c.StreamHTTP = &http.Client{Transport: tr}

	if _, _, _, err := c.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(chatBody)); err == nil {
		t.Fatalf("证书错误必须报错")
	} else if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("错误原因被改写了：%v", err)
	}
	if got := tr.calls.Load(); got != 1 {
		t.Fatalf("证书错误被重试了 %d 次（必须只 1 次）", got)
	}
}

// TestChatStreamExhaustedReturnsLastError 三次都断 → 把最后一次错误交给上层（不吞掉、不改成模糊文案）。
func TestChatStreamExhaustedReturnsLastError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := New()
	c.AgentHost = srv.URL
	c.HTTP = srv.Client()
	c.StreamHTTP = srv.Client()

	rc, _, _, err := c.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(chatBody))
	if rc != nil {
		_ = rc.Close()
	}
	if err == nil {
		t.Fatalf("全断时必须返回错误")
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("upstream 请求数=%d want 3（重试预算用尽）", got)
	}
}

// TestWiringDoesNotPanicOnCancel 失败分支里的 cancel() 不得因为拿到 nil 而 panic。
//
// httpretry.Do 失败时真 ctx 已自行 cancel，故返回一个空操作；渠道侧代码仍原样
// 调 cancel()。这条用例把「返回 nil 会炸」变成可回归的断言。
func TestWiringDoesNotPanicOnCancel(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid", nil)
	_, cancel, err := httpretry.Do(req, func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("lookup example.invalid: no such host")
	}, nil, nil)
	if err == nil {
		t.Fatalf("全失败应返回错误")
	}
	if !httpretry.Retryable(err) {
		t.Fatalf("no such host 必须可重试：%v", err)
	}
	// 关键：失败时返回的 cancel 不能是 nil —— 渠道侧错误分支无条件调它。
	if cancel == nil {
		t.Fatalf("失败分支返回了 nil cancel，渠道侧 cancel() 会 panic")
	}
	cancel() // 不得 panic
	if idle.DefaultTimeout <= 0 {
		t.Fatalf("前提失效")
	}
}

// recordingTripper 记录被调用次数并固定返回给定错误。
type recordingTripper struct {
	calls atomic.Int32
	err   error
}

func (r *recordingTripper) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls.Add(1)
	return nil, r.err
}
