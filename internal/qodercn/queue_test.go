package qodercn

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"omnigate/internal/auth"
	"omnigate/internal/provider"
)

// realQueueChunk 复刻 2026-10-20 实盘抓到的排队帧（上游三层嵌套 JSON）。
// 上游原始形态：{"code":"403","message":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":true,...}\"}"}
func realQueueChunk() map[string]any {
	inner := map[string]any{
		"isQueued": true, "modelKey": "dfmodel", "queueCount": 3058,
		"queueType": "p3", "retryAfterSeconds": 30, "serviceAvailable": true, "waitTime": 185,
	}
	mid := map[string]any{"code": "10605", "message": string(mustJSON(inner))}
	return map[string]any{"code": "403", "message": string(mustJSON(mid))}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// TestParseQueueFrameReal 排队帧识别：字段齐全、层级嵌套也要抽出来。
func TestParseQueueFrameReal(t *testing.T) {
	q := parseQueueFrame(realQueueChunk())
	if q == nil {
		t.Fatal("排队帧未被识别")
	}
	if q.ModelKey != "dfmodel" || q.QueueCount != 3058 || q.WaitSeconds != 185 || q.RetryAfter != 30 {
		t.Fatalf("字段解析错误：%+v", q)
	}
	// Kind 必须是 passthrough：排队不是账号故障，不得冷却账号。
	if got := q.Kind(); got != provider.ErrPassthrough {
		t.Fatalf("Kind = %s, want passthrough", got)
	}
}

// TestParseQueueFrameNegative 正常 chunk 不能被误判成排队。
func TestParseQueueFrameNegative(t *testing.T) {
	normal := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hi"}}}}
	if q := parseQueueFrame(normal); q != nil {
		t.Fatalf("正常 chunk 被误判为排队：%+v", q)
	}
	// 别的业务码（如 10606）不在此列。
	other := map[string]any{"code": "403", "message": `{"code":"10606","isQueued":false}`}
	if q := parseQueueFrame(other); q != nil {
		t.Fatalf("非排队业务码被误判：%+v", q)
	}
}

// TestClassifyModelQueued 排队类错误必须判 passthrough（不冷却账号）。
func TestClassifyModelQueued(t *testing.T) {
	body := `{"error":{"message":"模型排队中","type":"upstream_queued","code":"model_queued"}}`
	if got := Classify(http.StatusTooManyRequests, body); got != provider.ErrPassthrough {
		t.Fatalf("Classify(model_queued) = %s, want passthrough", got)
	}
	raw := string(mustJSON(realQueueChunk()))
	if got := Classify(http.StatusTooManyRequests, raw); got != provider.ErrPassthrough {
		t.Fatalf("Classify(10605 原始帧) = %s, want passthrough", got)
	}
	// 普通 429 仍是 soft_rate（不受本改动影响）。
	if got := Classify(http.StatusTooManyRequests, "too many requests"); got != provider.ErrSoftRate {
		t.Fatalf("Classify(普通 429) = %s, want soft_rate", got)
	}
}

// TestStreamQueueEmitsErrorFrame 流内排队帧 → 发 OpenAI error 帧且不补 [DONE]，
// 并返回 *QueueError（旧实现会把它当普通 chunk 透传，客户端干等 180 秒）。
func TestStreamQueueEmitsErrorFrame(t *testing.T) {
	frames := "data: " + string(mustJSON(map[string]any{
		"body": `{"id":"x","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
	})) + "\n\n" +
		"data: " + string(mustJSON(map[string]any{"body": string(mustJSON(realQueueChunk()))})) + "\n\n"

	rec := httptest.NewRecorder()
	_, err := Stream(rec, strings.NewReader(frames), "deepseek-flash")
	var q *QueueError
	if !asQueueError(err, &q) {
		t.Fatalf("err = %v, want *QueueError", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "model_queued") || !strings.Contains(out, "upstream_queued") {
		t.Fatalf("缺少 OpenAI error 帧：%s", out)
	}
	if strings.Contains(out, "[DONE]") {
		t.Fatalf("排队不得伪装成正常收尾：%s", out)
	}
}

// TestStreamQueueFrameBothEnvelopes 排队帧的两种信封形态都必须被识别（R46）：
//
//	A 嵌套：data:{"body":"<排队帧>"}   ← 抓包确认真实上游用这种
//	B 扁平：data:<排队帧>              ← 未抓到，但原始观测里出现过此形态，必须兜底
//
// 只支持一种是盲区：另一种会整帧被丢弃 → 客户端既无 error 又被补 [DONE]。
func TestStreamQueueFrameBothEnvelopes(t *testing.T) {
	frame := string(mustJSON(realQueueChunk()))
	cases := map[string]string{
		"nested": "data:" + string(mustJSON(map[string]any{"body": frame})) + "\n\n",
		"flat":   "data:" + frame + "\n\n",
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			usage, err := streamAsOpenAI(&buf, strings.NewReader(line), "deepseek-flash", nil)
			if err == nil {
				t.Fatalf("排队帧未识别：err=nil，输出=%s", buf.String())
			}
			var q *QueueError
			if !asQueueError(err, &q) {
				t.Fatalf("错误类型应为 *QueueError，got %T: %v", err, err)
			}
			if q.QueueCount != 3058 || q.WaitSeconds != 185 {
				t.Fatalf("队列信息解析错：%+v", q)
			}
			out := buf.String()
			if !strings.Contains(out, "model_queued") {
				t.Fatalf("缺 error 帧：%s", out)
			}
			if strings.Contains(out, "[DONE]") {
				t.Fatalf("排队中止后不得补 [DONE]（客户端会当正常收尾）：%s", out)
			}
			if usage != nil {
				t.Fatalf("排队帧不应产生 usage：%+v", usage)
			}
		})
	}
}

// TestStreamFlatNoiseNotTreatedAsError 扁平兜底不得把无关帧当错误透传，
// 正常嵌套帧仍须照常透传并补 [DONE]。
func TestStreamFlatNoiseNotTreatedAsError(t *testing.T) {
	var buf bytes.Buffer
	frames := "data:" + string(mustJSON(map[string]any{"body": `{"choices":[{"delta":{"content":"hi"}}]}`})) + "\n\n"
	if _, err := streamAsOpenAI(&buf, strings.NewReader(frames), "deepseek-flash", nil); err != nil {
		t.Fatalf("正常嵌套帧被判错：%v", err)
	}
	if strings.Contains(buf.String(), "model_queued") {
		t.Fatalf("正常帧被误判为排队：%s", buf.String())
	}
	if !strings.Contains(buf.String(), "[DONE]") {
		t.Fatalf("正常结束应补 [DONE]：%s", buf.String())
	}
}

// asQueueError 是 errors.As 的薄包装（避免在测试里再引 errors 包）。
func asQueueError(err error, target **QueueError) bool {
	for err != nil {
		if q, ok := err.(*QueueError); ok {
			*target = q
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// TestChatStreamFailsFastWhenQueued 观察到排队后，同模型请求应毫秒级返回 429，
// 不再陪上游挂 180 秒；且**不**调用上游（server 计数器必须为 0）。
func TestChatStreamFailsFastWhenQueued(t *testing.T) {
	called := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL, srv.URL)
	c.queueHoldBudget = time.Nanosecond // 本用例断言的是 R46 快速失败语义，关保活（R55 窗口动态化后默认预算会转入排队）
	c.rememberQueue("deepseek-flash", &QueueError{ModelKey: "dfmodel", QueueCount: 3058, WaitSeconds: 185, RetryAfter: 30})

	rc, status, body, err := c.ChatStream(fakeAuth(), []byte(`{"model":"deepseek-flash","messages":[]}`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if rc != nil {
		t.Fatalf("熔断分支不应返回流")
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", status)
	}
	if !bytes.Contains(body, []byte("model_queued")) || !bytes.Contains(body, []byte("3058")) {
		t.Fatalf("body 缺少排队信息：%s", body)
	}
	if called != 0 {
		t.Fatalf("熔断分支不得请求上游，却调用了 %d 次", called)
	}
	// 非排队模型不受影响。
	if q, _ := c.queueBlocked("qwen3.8-flash"); q != nil {
		t.Fatalf("其它模型被误熔断：%+v", q)
	}
}

// fakeAuth 测试用账号：必须带机器指纹，否则 NewCosySession 直接报错（走不到 HTTP 层）。
func fakeAuth() *auth.Auth {
	return &auth.Auth{UID: "u1", MachineID: "m1", MachineToken: "t1", MachineType: "windows"}
}

// TestChatStreamFirstByteWatchdog 上游响应头 200 但 body 一直不给数据
// （排队时的真实形态）→ 看门狗超时后必须返回 429 model_queued 而不是挂住。
func TestChatStreamFirstByteWatchdog(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush() // 头已写出，但 body 挂住
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	c := NewWithBase(srv.URL, srv.URL)
	c.FirstByteTimeout = 120 * time.Millisecond
	c.queueHoldBudget = time.Nanosecond // R55：关保活重投，保持「看门狗超时即 429」的 R46 断言语义

	start := time.Now()
	rc, status, body, err := c.ChatStream(fakeAuth(), []byte(`{"model":"deepseek-flash","messages":[]}`))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if rc != nil {
		t.Fatalf("看门狗分支不应返回流")
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", status)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("看门狗未生效，耗时 %v", elapsed)
	}
	if !bytes.Contains(body, []byte("model_queued")) {
		t.Fatalf("body 应标记 model_queued：%s", body)
	}
	// 熔断已写入：同模型下一次请求必须毫秒级失败，且不再打上游。
	if q, remain := c.queueBlocked("deepseek-flash"); q == nil || !q.Hang {
		t.Fatalf("看门狗超时后应熔断该模型（Hang=true），got %+v", q)
	} else if remain <= 0 {
		t.Fatalf("剩余熔断时间应 > 0，got %v", remain)
	}
}

// TestChatStreamWatchdogPassesNormalStream 首帧正常到达时，看门狗必须放行，
// 且预读进 bufio 的字节不能丢（否则客户端收不到第一个 chunk）。
func TestChatStreamWatchdogPassesNormalStream(t *testing.T) {
	const first = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, first)
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL, srv.URL)
	c.FirstByteTimeout = 2 * time.Second

	rc, status, _, err := c.ChatStream(fakeAuth(), []byte(`{"model":"deepseek-flash","messages":[]}`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if rc == nil || status != http.StatusOK {
		t.Fatalf("正常流应放行，status=%d rc=%v", status, rc)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.HasPrefix(got, []byte(first)) {
		t.Fatalf("预读的字节丢失：%q", string(got))
	}
	if q, _ := c.queueBlocked("deepseek-flash"); q != nil {
		t.Fatalf("正常流不应触发熔断：%+v", q)
	}
}

// TestChatStreamHTTP429QueueRecordsBlock 回归 R46 第三路径（2026-10-21 18:04 生产）：
// 上游直接快速回 HTTP 429 + 排队 body 时，同样必须写熔断窗口——
// 此前该路径只透传不熔断，客户端每次重试都要重新打上游（实测每跳 ~745ms）。
func TestChatStreamHTTP429QueueRecordsBlock(t *testing.T) {
	called := 0
	// 实盘形态：HTTP 429，body 为三层嵌套排队报文（扁平信封，无 "body" 字段）。
	queueJSON := string(mustJSON(realQueueChunk()))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, queueJSON)
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL, srv.URL)
	c.queueHoldBudget = time.Nanosecond // R55：关保活重投，保持 R46「透传原文+熔断」断言语义
	a := fakeAuth()
	body := []byte(`{"model":"deepseek-flash","messages":[]}`)

	// 第一跳：透传上游 429，同时熔断被写入。
	rc, status, respBody, err := c.ChatStream(a, body)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if rc != nil {
		rc.Close()
	}
	if status != http.StatusTooManyRequests || string(respBody) != queueJSON {
		t.Fatalf("第一跳应原文透传：status=%d body=%.120s", status, respBody)
	}
	if q, _ := c.queueBlocked("deepseek-flash"); q == nil || q.QueueCount != 3058 {
		t.Fatalf("HTTP 级排队应写熔断（含队列信息），got %+v", q)
	}
	if called != 1 {
		t.Fatalf("第一跳应只打上游一次，got %d", called)
	}

	// 第二跳：毫秒级快速失败，且不再打上游。
	before := called
	start := time.Now()
	rc2, st2, body2, _ := c.ChatStream(a, body)
	if rc2 != nil {
		rc2.Close()
	}
	if st2 != http.StatusTooManyRequests || !bytes.Contains(body2, []byte("model_queued")) {
		t.Fatalf("第二跳应为熔断 429：status=%d body=%.160s", st2, body2)
	}
	if called != before {
		t.Fatalf("熔断期内不得请求上游，却打了 %d 次", called-before)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("熔断快速失败应毫秒级，耗时 %v", time.Since(start))
	}
}

// ── R55 回归：熔断 key 归一化 + 排队保活重投 ──

// TestFuseKeyCrossNamespaceBlock 回归 R55-a 根因（2026-10-22 17:21 生产日志实锤）：
// Stream() 侧用客户端原始名（含渠道前缀与费率后缀）写熔断，ChatStream() 侧用
// rewriteModel 改写后的去前缀名查熔断 —— 修复前两端键空间不同，生产熔断 0 次命中。
func TestFuseKeyCrossNamespaceBlock(t *testing.T) {
	c := New()
	c.rememberQueue("qodercn/deepseek-flash (0.00x)", &QueueError{ModelKey: "dfmodel", QueueCount: 5630, WaitSeconds: 295, RetryAfter: 30})
	q, remain := c.queueBlocked("deepseek-flash")
	if q == nil || remain <= 0 {
		t.Fatal("带渠道前缀+费率后缀写入的熔断，必须能被去前缀名命中（同一键空间）")
	}
	if q.QueueCount != 5630 {
		t.Fatalf("命中了错误的熔断记录：%+v", q)
	}
	// 大小写、前缀、后缀变体都归一到同一键；别的模型不受牵连。
	if q2, _ := c.queueBlocked("QODERCN/DeepSeek-Flash (0.11x)"); q2 == nil {
		t.Fatal("大小写/变体应归一化命中")
	}
	if q3, _ := c.queueBlocked("qwen3.8-flash"); q3 != nil {
		t.Fatalf("其它模型被误熔断：%+v", q3)
	}
}

// envelopeLine 把内层 chunk 包进上游 body 信封成一行 SSE。
func envelopeLine(t *testing.T, inner any) string {
	t.Helper()
	return "data: " + string(mustJSON(map[string]any{"body": string(mustJSON(inner))})) + "\n\n"
}

// TestChatStreamHoldResubmitsMidStreamQueue R55 形态②：首跳 200 流、首帧即流内排队帧
// → 同一客户端请求内重投，二跳内容成功 → 客户端拿到 200 正常流，全程没见过排队帧。
func TestChatStreamHoldResubmitsMidStreamQueue(t *testing.T) {
	var calls int32
	content := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hi"}}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		if n == 1 {
			// 实盘形态：排队帧走扁平三层 JSON（R49 抓包）。
			io.WriteString(w, "data: "+string(mustJSON(realQueueChunk()))+"\n\n")
			f.Flush()
			return
		}
		io.WriteString(w, envelopeLine(t, content))
		io.WriteString(w, "data: "+string(mustJSON(map[string]any{"body": "[DONE]"}))+"\n\n")
		f.Flush()
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL, srv.URL)
	c.queueHoldBudget = 3 * time.Second
	c.queueHoldPause = 10 * time.Millisecond

	rc, status, body, err := c.ChatStream(fakeAuth(), []byte(`{"model":"deepseek-flash","messages":[]}`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if rc == nil || status != http.StatusOK {
		if rc != nil {
			rc.Close()
		}
		t.Fatalf("保活重投后应返回 200 流：status=%d body=%.200s", status, body)
	}
	defer rc.Close()
	out, _ := io.ReadAll(rc)
	if !bytes.Contains(out, []byte("hi")) {
		t.Fatalf("放行字节缺内容帧：%q", out)
	}
	if bytes.Contains(out, []byte("10605")) {
		t.Fatalf("排队帧应被保活消化，不该透传给客户端：%q", out)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("应恰好打上游 2 次（首跳排队、重投成功），got %d", n)
	}
}

// TestChatStreamHoldBudgetExhaust R55 预算耗尽回落 R46：每一跳都是排队帧 →
// 预算内重投若干次，耗尽后原样放行最后一跳的流（下游 Stream() 照旧出 error 帧），
// 熔断同时被写入。
func TestChatStreamHoldBudgetExhaust(t *testing.T) {
	var calls int32
	queueLine := "data: " + string(mustJSON(realQueueChunk())) + "\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, queueLine)
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL, srv.URL)
	c.queueHoldBudget = 150 * time.Millisecond
	c.queueHoldPause = 10 * time.Millisecond

	start := time.Now()
	rc, status, _, err := c.ChatStream(fakeAuth(), []byte(`{"model":"deepseek-flash","messages":[]}`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if rc == nil || status != http.StatusOK {
		t.Fatalf("预算耗尽应原样放行排队流交下游出 error 帧：status=%d", status)
	}
	defer rc.Close()
	out, _ := io.ReadAll(rc)
	if !bytes.Contains(out, []byte("10605")) {
		t.Fatalf("最后一跳的排队帧应原样透传：%q", out)
	}
	if n := atomic.LoadInt32(&calls); n < 2 {
		t.Fatalf("保活应重投过多跳，got %d", n)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("保活受预算上界约束，耗时 %v", elapsed)
	}
	if q, remain := c.queueBlocked("deepseek-flash"); q == nil || remain <= 0 {
		t.Fatal("排队期间必须写熔断，保护并发请求不陪等")
	}
}

// TestChatStreamHoldOnHTTP429 R55 形态①：首跳 HTTP 429 + 排队 body → 重投，
// 二跳 200 正常流 → 客户端拿到成功响应。
func TestChatStreamHoldOnHTTP429(t *testing.T) {
	var calls int32
	queueJSON := string(mustJSON(realQueueChunk()))
	content := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "ok"}}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, queueJSON)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, envelopeLine(t, content))
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL, srv.URL)
	c.queueHoldBudget = 3 * time.Second
	c.queueHoldPause = 10 * time.Millisecond

	rc, status, body, err := c.ChatStream(fakeAuth(), []byte(`{"model":"deepseek-flash","messages":[]}`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if rc == nil || status != http.StatusOK {
		if rc != nil {
			rc.Close()
		}
		t.Fatalf("429 排队快速拒绝应在预算内重投成功：status=%d body=%.200s", status, body)
	}
	defer rc.Close()
	out, _ := io.ReadAll(rc)
	if !bytes.Contains(out, []byte("ok")) {
		t.Fatalf("重投成功但缺内容：%q", out)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("应恰好打上游 2 次，got %d", n)
	}
}

// TestFuseWindowBoundedByHoldBudget 回归 R55c（2026-10-22 20:00 生产）：
// 熔断窗口必须动态且**严格小于保活预算** —— 10min 固定窗被保活循环每跳续期后，
// 剩余永远 ~578s > 预算 300s，DSH 的等待请求被永久锁在门外 10ms 硬拒。
func TestFuseWindowBoundedByHoldBudget(t *testing.T) {
	if w := fuseWindow(&QueueError{WaitSeconds: 36}); w != time.Minute+360*time.Second/10 {
		t.Fatalf("waitTime=36 应为 96s，got %v", w)
	}
	if w := fuseWindow(&QueueError{WaitSeconds: 10}); w != queueHangWindow {
		t.Fatalf("小 waitTime 应夹到下界 %v，got %v", queueHangWindow, w)
	}
	if w := fuseWindow(&QueueError{WaitSeconds: 480}); w != queueBlockWindow {
		t.Fatalf("大 waitTime 应夹到上界 %v，got %v", queueBlockWindow, w)
	}
	if queueBlockWindow >= defaultQueueHoldBudget {
		t.Fatalf("熔断上界 %v 必须小于保活预算 %v，否则续期后的熔断会把等待者永久锁在门外",
			queueBlockWindow, defaultQueueHoldBudget)
	}
	// 写入侧同样走动态窗：排队帧 185s → 245s，而非旧固定 10min。
	c := New()
	c.rememberQueue("some-model", &QueueError{ModelKey: "m", WaitSeconds: 185, QueueCount: 3058})
	if q, remain := c.queueBlocked("some-model"); q == nil || remain > 246*time.Second || remain < 200*time.Second {
		t.Fatalf("熔断窗口应≈waitTime+60s（245s），got %v", remain)
	}
}

// TestChatStreamEntryFuseWithinBudgetHolds R55c：熔断剩余在预算内时，新请求
// 不在门口被拒也不干等，直接进保活循环按节奏排队 —— 上游恢复即成功。
func TestChatStreamEntryFuseWithinBudgetHolds(t *testing.T) {
	var calls int32
	content := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "late"}}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if n == 1 {
			io.WriteString(w, "data: "+string(mustJSON(realQueueChunk()))+"\n\n")
		} else {
			io.WriteString(w, envelopeLine(t, content))
		}
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL, srv.URL)
	c.queueHoldBudget = 3 * time.Minute // 大于熔断剩余 90s：进入「预算内转保活」分支
	c.queueHoldPause = 10 * time.Millisecond
	// 先写一个「剩余在预算内」的熔断（waitTime=30 → 窗口 90s）。
	c.rememberQueue("deepseek-flash", &QueueError{ModelKey: "dfmodel", QueueCount: 100, WaitSeconds: 30, RetryAfter: 30})

	rc, status, body, err := c.ChatStream(fakeAuth(), []byte(`{"model":"deepseek-flash","messages":[]}`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if rc == nil || status != http.StatusOK {
		if rc != nil {
			rc.Close()
		}
		t.Fatalf("熔断在预算内应转入保活排队并最终成功：status=%d body=%.200s", status, body)
	}
	defer rc.Close()
	out, _ := io.ReadAll(rc)
	if !bytes.Contains(out, []byte("late")) {
		t.Fatalf("缺内容帧：%q", out)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("入口未拒 + 一跳排队 + 二跳成功 = 2 次，got %d", n)
	}
}
