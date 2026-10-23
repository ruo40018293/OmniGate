// R56 回归：qodercn 等待期进度上报。
//
// 锁住的核心风险是**串号**：一个 Client 并发服务多个请求，如果把进度挂成
// 单一全局回调，A 请求的「前方 5630 个」会显示到 B 请求的面板行上，用户会
// 据此误判「我的请求在排 5630 位」。所以这里按 body 隔离来做断言。
package qodercn

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"omnigate/internal/provider"
)

// 同一 body 注册后能收到进度；不同 body 收不到（并发不串号）。
func TestProgressSinkIsolatedPerBody(t *testing.T) {
	c := NewWithBase("http://x", "http://x")
	bodyA := []byte(`{"model":"deepseek-flash","messages":[{"role":"user","content":"a"}]}`)
	bodyB := []byte(`{"model":"deepseek-flash","messages":[{"role":"user","content":"b"}]}`)
	if progKey(bodyA) == progKey(bodyB) {
		t.Fatal("两个不同 body 的键撞了，按 body 隔离会失效")
	}

	var gotA, gotB int
	cancelA := c.SetProgressSink(bodyA, func(p provider.Progress) { gotA++ })
	cancelB := c.SetProgressSink(bodyB, func(p provider.Progress) { gotB++ })
	defer cancelB()

	c.ReportProgress(bodyA, provider.Progress{Phase: "queued", Pos: 5630})
	if gotA != 1 {
		t.Fatalf("bodyA 的进度应送到自己的回调，实际 %d 次", gotA)
	}
	if gotB != 0 {
		t.Fatalf("bodyB 不应收到 bodyA 的进度（串号），实际 %d 次", gotB)
	}

	// cancel 后不再收到
	cancelA()
	c.ReportProgress(bodyA, provider.Progress{Phase: "retrying"})
	if gotA != 1 {
		t.Fatalf("cancel 后不应再回调，实际 %d 次", gotA)
	}
}

// 未注册的 body：上报是安全的空操作（绝大多数渠道/请求都属于这种情况）。
func TestProgressSinkNoopWhenUnregistered(t *testing.T) {
	c := NewWithBase("http://x", "http://x")
	c.ReportProgress([]byte(`{"model":"m"}`), provider.Progress{Phase: "queued"})
	c.ReportProgress(nil, provider.Progress{Phase: "queued"}) // nil body 不得 panic
}

// 空 Client（nil 指针）不得 panic：测试与降级路径会出现。
func TestProgressSinkNilClientSafe(t *testing.T) {
	var c *Client
	c.ReportProgress([]byte(`{}`), provider.Progress{Phase: "queued"})
	// SetProgressSink 在 nil Client 上也必须返回可调用的 cancel
	if cancel := c.SetProgressSink([]byte(`{}`), func(provider.Progress) {}); cancel != nil {
		cancel()
	}
}

// 回调 panic 必须被吞掉：进度是旁路信息，绝不能把请求主流程带崩。
func TestProgressSinkSwallowsPanic(t *testing.T) {
	c := NewWithBase("http://x", "http://x")
	body := []byte(`{"model":"m"}`)
	c.SetProgressSink(body, func(provider.Progress) { panic("boom") })
	c.ReportProgress(body, provider.Progress{Phase: "queued"}) // 不应 panic 到本层
}

// 真实排队场景：上游 HTTP 429 + 排队 body → 保活重投时上报「排队」，且带出
// 上游真实的前方人数与等待秒数（面板显示的就是这两个数字）。
func TestChatStreamReportsQueueProgress(t *testing.T) {
	var calls int32
	queueJSON := string(mustJSON(realQueueChunk()))
	content := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "ok"}}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			// 第一跳：上游排队快速拒绝（实盘形态：HTTP 429 + 三层嵌套排队报文）
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, queueJSON)
			return
		}
		// 第二跳：正常出流
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, envelopeLine(t, content))
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL, srv.URL)
	c.queueHoldBudget = 5 * time.Second
	c.queueHoldPause = 10 * time.Millisecond // 测试里不等真实 30s

	body := []byte(`{"model":"deepseek-flash","messages":[]}`)
	var mu2 sync.Mutex
	var seen []provider.Progress
	c.SetProgressSink(body, func(p provider.Progress) {
		mu2.Lock()
		seen = append(seen, p)
		mu2.Unlock()
	})

	rc, status, _, err := c.ChatStream(fakeAuth(), body)
	if err != nil {
		t.Fatalf("ChatStream 错误: %v", err)
	}
	defer rc.Close()
	if status != http.StatusOK {
		t.Fatalf("第二跳应 200，实际 %d", status)
	}

	mu2.Lock()
	defer mu2.Unlock()
	if len(seen) == 0 {
		t.Fatal("排队重投期间应至少上报一次进度，否则面板仍然看不见等待")
	}
	var found bool
	for _, p := range seen {
		if p.Phase == "queued" && p.Pos == 3058 && p.Wait == 185 {
			found = true
		}
	}
	if !found {
		t.Fatalf("应上报上游真实的排队信息（前方 3058 / 等待 185s），实际事件: %+v", seen)
	}
}
