// r93_contentwatch_test.go 锁定 R93 的「首内容截止」看门狗。
//
// 背景（R90/R92 实测）：Qoder 系渠道当日额度耗尽时，上游先发一帧 role（content 为空）
// 就完全沉默，直到约 180 秒才自行断开。这段时间 handler 既不报错也不换号，用户界面上
// 只有一个计时器 —— 「卡住了」的体感就是这么来的。
//
// 本测试锁三件事：
//  1. 判定「内容」时不能把 role 首帧（content 为空串）算成内容，否则看门狗永远不触发；
//  2. content / reasoning_content / tool_calls 三种形态都算内容（渠道不同、模型不同）；
//  3. 沉默上游必须在截止时间附近被主动断开，而不是等上游自己那 180 秒。
package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"omnigate/internal/auth"
	"omnigate/internal/pool"
	"omnigate/internal/provider"
)

func TestHasContentFrame(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"role 首帧不算内容", `data: {"choices":[{"delta":{"role":"assistant","content":""}}]}`, false},
		{"空字符串结束符", `"content":""`, false},
		{"正文算内容", `data: {"choices":[{"delta":{"content":"你好"}}]}`, true},
		{"单字符也算", `"content":"a"`, true},
		{"思考正文算内容", `data: {"choices":[{"delta":{"reasoning_content":"想"}}]}`, true},
		{"空思考不算", `"reasoning_content":""`, false},
		{"工具调用算内容", `data: {"choices":[{"delta":{"tool_calls":[{"id":"c1"}]}}]}`, true},
		{"空工具数组不算", `"tool_calls":[]`, false},
		{"null 不算", `"tool_calls":null`, false},
		{"工具数组带空白", `"tool_calls": [ {"id":"c1"} ]`, true},
		{"纯注释帧不算", ": omnigate request accepted\n\n", false},
		{"[DONE] 不算", `data: [DONE]`, false},
		{"空字节", "", false},
		{"多帧中有一帧有内容", "data: {\"choices\":[{\"delta\":{\"content\":\"\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n", true},
	}
	for _, c := range cases {
		if got := hasContentFrame([]byte(c.in)); got != c.want {
			t.Errorf("%s: hasContentFrame=%v want %v (%q)", c.name, got, c.want, c.in)
		}
	}
}

func TestContentWatcherSignalsOnce(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := newContentWatcher(rec)
	if cw.HasContent() {
		t.Fatal("新包装器不该已看到内容")
	}
	if _, err := io.WriteString(cw, `data: {"choices":[{"delta":{"role":"assistant","content":""}}]}`); err != nil {
		t.Fatalf("写 role 帧失败: %v", err)
	}
	if cw.HasContent() {
		t.Fatal("role 首帧（content 为空）不得被算成内容")
	}
	select {
	case <-cw.Seen():
		t.Fatal("Seen 不该已关闭")
	default:
	}
	if _, err := io.WriteString(cw, `data: {"choices":[{"delta":{"content":"hi"}}]}`); err != nil {
		t.Fatalf("写内容帧失败: %v", err)
	}
	if !cw.HasContent() {
		t.Fatal("内容帧到达后 HasContent 必须为真")
	}
	select {
	case <-cw.Seen():
	case <-time.After(time.Second):
		t.Fatal("内容帧到达后 Seen 必须在 1s 内关闭")
	}
	if got := rec.Body.String(); !strings.Contains(got, `"content":"hi"`) {
		t.Fatalf("包装器必须透传字节，实际 %q", got)
	}
}

// silentUpstream 假上游：ChatStream 返回一个永远读不到数据的流；
// Stream 先写 role 首帧，然后阻塞在读取上，直到 body 被 Close。
type silentUpstream struct {
	writeRole bool
	content   string
	closed    atomic.Bool
	readErr   atomic.Value // error
}

func (u *silentUpstream) RefreshToken(*auth.Auth) error { return nil }
func (u *silentUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) {
	return nil, nil
}
func (u *silentUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}
func (u *silentUpstream) UserResource(*auth.Auth) (int64, error) { return 0, nil }
func (u *silentUpstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (u *silentUpstream) DailyCheckin(*auth.Auth) error { return nil }
func (u *silentUpstream) Classify(int, string) provider.ErrKind {
	return provider.ErrNone
}

func (u *silentUpstream) ChatStream(*auth.Auth, []byte) (io.ReadCloser, int, []byte, error) {
	pr, pw := io.Pipe()
	// 保持 pw 引用，避免被 GC 前就关闭；真正让 Read 返回的是 pr.CloseWithError。
	_ = pw
	return &watchedReader{ReadCloser: pr, up: u}, http.StatusOK, nil, nil
}

func (u *silentUpstream) Stream(w http.ResponseWriter, r io.Reader, _ string) (map[string]any, error) {
	if u.writeRole {
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n")
	}
	if u.content != "" {
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\""+u.content+"\"}}]}\n\n")
		return map[string]any{}, nil
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	// 模拟「上游沉默」：一直读到 body 被关闭为止。
	_, err := io.Copy(io.Discard, r)
	if err == nil {
		err = errors.New("upstream stream truncated")
	}
	u.readErr.Store(err)
	provider.WriteTruncationFrame(w, err)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil, provider.TruncationError(err)
}

func (u *silentUpstream) Aggregate(io.Reader, string) (map[string]any, error) {
	return map[string]any{"choices": []any{}}, nil
}

type watchedReader struct {
	io.ReadCloser
	up   *silentUpstream
	once sync.Once
}

func (w *watchedReader) Close() error {
	w.once.Do(func() { w.up.closed.Store(true) })
	return w.ReadCloser.Close()
}

func newWatchHandler(t *testing.T, up *silentUpstream, d time.Duration) *Handler {
	t.Helper()
	p := pool.New(t.TempDir() + "/state.json")
	p.Add(&auth.Auth{Kind: string(singleAcctKind), AccessToken: "public",
		ExpiresAt: 4102444800, UID: singleAcctUID})
	return NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			singleAcctKind: {Kind: singleAcctKind, Pool: p, Upstream: up, SingleAccount: true},
		},
		APIKey: "k", MaxRotate: 1,
		HardCooldown: time.Hour, SoftCooldown: time.Minute,
		ErrThreshold: 1, ErrCooldown: time.Minute,
		FirstContentTimeout: d,
	})
}

func postStream(t *testing.T, h *Handler) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"singleacct/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// 核心用例：上游只发 role 首帧就沉默 ⇒ 看门狗在截止时间附近断开，
// 并在响应里给出 upstream_timeout（而不是让客户端等上游自己那 180 秒）。
func TestFirstContentTimeoutAbortsSilentUpstream(t *testing.T) {
	up := &silentUpstream{writeRole: true}
	h := newWatchHandler(t, up, 80*time.Millisecond)

	start := time.Now()
	rec := postStream(t, h)
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("看门狗没生效：请求耗时 %s（应≈80ms）", elapsed)
	}
	if !up.closed.Load() {
		t.Fatal("看门狗必须关闭上游 body，否则读取会一直阻塞")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "upstream_timeout") {
		t.Fatalf("主动断流必须带 upstream_timeout 码，实际响应：%q", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("超时不得伪装成正常收尾：%q", body)
	}
}

// 内容按时到达 ⇒ 看门狗不得开枪（否则会把能用的流砍掉）。
func TestFirstContentArrivesBeforeDeadline(t *testing.T) {
	up := &silentUpstream{writeRole: true, content: "好"}
	h := newWatchHandler(t, up, 500*time.Millisecond)

	rec := postStream(t, h)
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"好"`) {
		t.Fatalf("内容必须透传给客户端，实际：%q", body)
	}
	if strings.Contains(body, "upstream_timeout") {
		t.Fatalf("内容已到达却仍报超时：%q", body)
	}
}

// 关闭看门狗（<=0）时保持旧行为：不做首内容判断。
// 这里只断言不 panic、内容照常透传（沉默场景交给 idle 监控兜底）。
func TestFirstContentWatchdogDisabled(t *testing.T) {
	up := &silentUpstream{writeRole: true, content: "x"}
	h := newWatchHandler(t, up, 0)
	rec := postStream(t, h)
	if !strings.Contains(rec.Body.String(), `"content":"x"`) {
		t.Fatalf("看门狗关闭后内容仍须透传，实际：%q", rec.Body.String())
	}
}
