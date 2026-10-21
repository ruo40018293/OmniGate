package qodercn

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
