// R56b 回归：等待期可见性。
//
// 事故（2026-10-23 15:51）：上游 200 后 180 秒零内容，客户端只有一个计时器，
// 用户无法判断「在等」还是「卡死」。三处修复各配一个断言，删掉任一处都应变红：
//  1. 受理帧：任何流式响应开头发一帧 SSE 注释并 flush；
//  2. 扁平错误帧：非排队的错误原文（如 code 110 计费超限）必须下传，不能丢弃；
//  3. 内容时刻：MarkContent 只由真实内容触发，且幂等。
package qodercn

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// syncBuf 是并发安全的写入缓冲：活动探针会在另一个 goroutine 里写。
type syncBuf struct {
	mu  chan struct{}
	buf bytes.Buffer
}

func newSyncBuf() *syncBuf {
	s := &syncBuf{mu: make(chan struct{}, 1)}
	s.mu <- struct{}{}
	return s
}

func (s *syncBuf) Write(p []byte) (int, error) {
	<-s.mu
	defer func() { s.mu <- struct{}{} }()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	<-s.mu
	defer func() { s.mu <- struct{}{} }()
	return s.buf.String()
}

func (s *syncBuf) Flush() {}

// TestStreamCaptureDoesNotDuplicateAcceptedFrame：受理帧的**唯一**责任方是上层
// acceptWriter（internal/server/handler.go）。
//
// 历史（2026-10-23 19:53）：qodercn 内部也发过一帧，结果客户端收到两帧，且 handler
// 侧先写会把响应头定成 text/plain 导致 SSE 解析失败。这条测试锁死「本层不再发」，
// 防止将来有人在这里再加一次。
func TestStreamCaptureDoesNotDuplicateAcceptedFrame(t *testing.T) {
	buf := newSyncBuf()
	w := &flushRecorder{ResponseWriter: &nopResponseWriter{h: http.Header{}, w: buf}, flushed: new(int)}
	body := envelopeLine(t, `{"choices":[{"delta":{"content":"hi"}}]}`) + "data: {\"body\":\"[DONE]\"}\n\n"
	if _, err := StreamCapture(w, strings.NewReader(body), "deepseek-flash", nil); err != nil {
		t.Fatalf("StreamCapture 返回错误：%v", err)
	}
	if n := strings.Count(buf.String(), ": omnigate request accepted"); n != 0 {
		t.Fatalf("渠道层不得自行发受理帧（由 acceptWriter 统一负责），实际发了 %d 次", n)
	}
}

// TestStreamCaptureSetsSSEHeaders：渠道层仍须保证 SSE 响应头（独立使用时的契约）。
func TestStreamCaptureSetsSSEHeaders(t *testing.T) {
	buf := newSyncBuf()
	w := &flushRecorder{ResponseWriter: &nopResponseWriter{h: http.Header{}, w: buf}, flushed: new(int)}
	StreamCapture(w, strings.NewReader(""), "m", nil)
	h := w.Header()
	if h.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("Content-Type 应为 text/event-stream，实际=%q", h.Get("Content-Type"))
	}
	if h.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("应设 X-Accel-Buffering: no 防止代理攒包，实际=%q", h.Get("X-Accel-Buffering"))
	}
}

// TestFlatErrorFramePassesThrough扁平错误帧必须原样下传（不改写、不吞掉）。
//
// 事故原文：`data: {"code":"110","message":"Billing daily count exceeded"}`。
// 本测试锁定**行为契约**（parseNestedSSE 的扁平帧兜底负责下传）；它保护的是
// 「别把这条兜底路径删掉」——删掉后客户端会在 180 秒里什么都收不到。
func TestFlatErrorFramePassesThrough(t *testing.T) {
	var buf bytes.Buffer
	frames := "data: {\"code\":\"110\",\"message\":\"Billing daily count exceeded\",\"model\":\"deepseek-flash\"}\n\n"
	_, err := streamAsOpenAI(&buf, strings.NewReader(frames), "deepseek-flash", nil, nil)
	if err != nil {
		t.Fatalf("扁平错误帧不应导致解析失败：%v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Billing daily count exceeded") {
		t.Fatalf("计费超限原文必须下传给客户端（否则用户只看到空流），实际=%q", out)
	}
	if !strings.Contains(out, "\"code\":\"110\"") {
		t.Fatalf("错误码应保留，实际=%q", out)
	}
}

// TestFlatErrorFrameTriggersContent错误原文也算「用户看到的内容」，应触发 onContent。
//
// 这一条是 R56b 的真修复点：就算帧下传了，只要面板还显示「还在等首字」，
// 用户依然不知道发生了什么。
func TestFlatErrorFrameTriggersContent(t *testing.T) {
	var buf bytes.Buffer
	called := 0
	frames := "data: {\"code\":\"110\",\"message\":\"Billing daily count exceeded\"}\n\n"
	streamAsOpenAI(&buf, strings.NewReader(frames), "m", nil, func() { called++ })
	if called != 1 {
		t.Fatalf("错误原文下传后应触发一次内容通知，实际=%d", called)
	}
}

// TestContentCallbackOnceForMultipleChunks：多个内容帧只通知一次。
func TestContentCallbackOnceForMultipleChunks(t *testing.T) {
	var buf bytes.Buffer
	called := 0
	frames := "data: {\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"a\\\"}}]}\"}\n\n" +
		"data: {\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"b\\\"}}]}\"}\n\n" +
		"data: {\"body\":\"[DONE]\"}\n\n"
	StreamCaptureWithContent(&nopRW{h: http.Header{}, w: &buf}, strings.NewReader(frames), "m", nil, func() { called++ })
	if called != 1 {
		t.Fatalf("首个内容帧只通知一次，实际=%d", called)
	}
}

// TestActivityWatchMarksClientDead：客户端写失败后探针应停止并标记。
func TestActivityWatchMarksClientDead(t *testing.T) {
	a := &activityWatch{w: errWriter{}, flush: func() {}, stop: make(chan struct{})}
	a.touch()
	a.loop(20 * time.Millisecond)
	if !a.dead() {
		t.Fatalf("写入失败后应判定客户端已断开")
	}
	rep := a.report()
	if rep["client_dead"] != true {
		t.Fatalf("取证字段应记录 client_dead=true，实际=%v", rep)
	}
}

// TestActivityWatchSilentForBuckets：沉默时长随时间增长。
func TestActivityWatchSilentForBuckets(t *testing.T) {
	a := &activityWatch{w: io.Discard, flush: func() {}, stop: make(chan struct{})}
	a.touch()
	a.lastWrite.Store(time.Now().Add(-45 * time.Second).UnixNano())
	if d := a.SilentFor(); d < 30*time.Second {
		t.Fatalf("沉默时长应约 45s，实际=%v", d)
	}
}

// ---- 测试替身 ----

type nopResponseWriter struct {
	h http.Header
	w io.Writer
}

func (n *nopResponseWriter) Header() http.Header         { return n.h }
func (n *nopResponseWriter) WriteHeader(int)             {}
func (n *nopResponseWriter) Write(p []byte) (int, error) { return n.w.Write(p) }
func (n *nopResponseWriter) Flush()                      {}

type nopRW struct {
	h http.Header
	w io.Writer
}

func (n *nopRW) Header() http.Header         { return n.h }
func (n *nopRW) WriteHeader(int)             {}
func (n *nopRW) Write(p []byte) (int, error) { return n.w.Write(p) }
func (n *nopRW) Flush()                      {}

type flushRecorder struct {
	http.ResponseWriter
	flushed *int
}

func (f *flushRecorder) Flush() {
	*f.flushed++
	if fl, ok := f.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}
func (f *flushRecorder) Write(p []byte) (int, error) {
	return f.ResponseWriter.Write(p)
}
func (f *flushRecorder) WriteHeader(c int) { f.ResponseWriter.WriteHeader(c) }

// errWriter 模拟「客户端已断开」。
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

var _ = httptest.NewRecorder // 保留 httptest 依赖，后续用真实 recorder 校验时不必再改 import
