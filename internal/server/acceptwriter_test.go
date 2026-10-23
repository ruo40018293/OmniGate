// R56b 回归：受理帧包装器。
//
// 事故（2026-10-23 19:53）：直接裸写 w 发受理帧 → 响应头被永久定成 text/plain，
// SSE 客户端拒绝解析，且与渠道内部那帧重复成两帧。这组测试锁死正确顺序。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAcceptWriterSetsSSEHeaderBeforeBody：受理帧之前必须先设好 event-stream。
func TestAcceptWriterSetsSSEHeaderBeforeBody(t *testing.T) {
	rec := httptest.NewRecorder()
	aw := newAcceptWriter(rec)
	if _, err := aw.Write([]byte("data: x\n\n")); err != nil {
		t.Fatal(err)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("受理帧前必须先把 Content-Type 设为 text/event-stream，实际=%q", ct)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, ": omnigate request accepted") {
		t.Fatalf("正文第一帧应是受理注释帧，实际开头=%q", firstN(body, 60))
	}
	if !strings.Contains(body, "data: x") {
		t.Fatalf("真实数据应跟在受理帧之后，实际=%q", body)
	}
}

// TestAcceptWriterSendsFrameExactlyOnce：只发一次（曾因 handler+渠道双发出现两帧）。
func TestAcceptWriterSendsFrameExactlyOnce(t *testing.T) {
	rec := httptest.NewRecorder()
	aw := newAcceptWriter(rec)
	aw.Write([]byte("a"))
	aw.Write([]byte("b"))
	aw.Flush()
	body := rec.Body.String()
	if n := strings.Count(body, ": omnigate request accepted"); n != 1 {
		t.Fatalf("受理帧必须只发一次，实际=%d 次，正文=%q", n, body)
	}
}

// TestAcceptWriterKeepsChannelContentType：渠道自己设过 Content-Type 时不覆盖。
//
// 场景：某些渠道用 application/json 发错误体，硬改成 event-stream 会让客户端误判。
func TestAcceptWriterKeepsChannelContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/json")
	aw := newAcceptWriter(rec)
	aw.Write([]byte(`{"error":1}`))
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("渠道已设的 Content-Type 不应被覆盖，实际=%q", ct)
	}
}

// TestAcceptWriterFlushes：受理帧之后必须 flush，否则客户端看不到。
func TestAcceptWriterFlushes(t *testing.T) {
	rec := &flushCounter{ResponseWriter: httptest.NewRecorder()}
	aw := newAcceptWriter(rec)
	aw.Write([]byte("x"))
	if rec.flushes == 0 {
		t.Fatalf("受理帧后必须 flush，实际 flush 次数=0")
	}
}

// TestAcceptWriterWriteHeaderStillWorks：WriteHeader 透传且只触发一次受理帧。
func TestAcceptWriterWriteHeaderStillWorks(t *testing.T) {
	rec := httptest.NewRecorder()
	aw := newAcceptWriter(rec)
	aw.WriteHeader(http.StatusOK)
	aw.Write([]byte("z"))
	if n := strings.Count(rec.Body.String(), ": omnigate request accepted"); n != 1 {
		t.Fatalf("WriteHeader + Write 只应发一次受理帧，实际=%d", n)
	}
}

type flushCounter struct {
	http.ResponseWriter
	flushes int
}

func (f *flushCounter) Flush() { f.flushes++ }

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
