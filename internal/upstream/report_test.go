package upstream

// 行为上报原语（/v2/report）测试。
//
// 重点护栏：**请求体必须是 JSON 数组**。上游按数组解析，
// 单条事件也必须包一层 —— 发成裸对象会被上游拒绝，
// 而错误表现是「上报 200 但进度不点亮」，极难定位。
//
// 全部用例不发起真实网络请求。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"omnigate/internal/auth"
)

// captureReport 起一个测试服务器，捕获 /v2/report 的请求体。
func captureReport(t *testing.T, resp string) (*Client, *[]byte, func()) {
	t.Helper()
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != reportPath {
			t.Errorf("路径=%q want %q", r.URL.Path, reportPath)
		}
		if r.Method != http.MethodPost {
			t.Errorf("方法=%s want POST", r.Method)
		}
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	c := New()
	c.BillingBaseCN = srv.URL
	return c, &got, srv.Close
}

// 单条事件也必须以数组形式下发。
func TestReportEventsSendsJSONArray(t *testing.T) {
	c, got, done := captureReport(t, `{"code":0,"data":{}}`)
	defer done()

	err := c.ReportEvents(testAuth(), []map[string]any{
		{"eventCode": "chat_request_send", "userId": "u1"},
	})
	if err != nil {
		t.Fatalf("上报失败：%v", err)
	}

	var arr []map[string]any
	if err := json.Unmarshal(*got, &arr); err != nil {
		t.Fatalf("请求体不是 JSON 数组：%v（原文 %s）", err, string(*got))
	}
	if len(arr) != 1 {
		t.Fatalf("数组长度=%d want 1", len(arr))
	}
	if arr[0]["eventCode"] != "chat_request_send" {
		t.Fatalf("eventCode=%v", arr[0]["eventCode"])
	}
}

// 多条事件按给定顺序原样下发（顺序即上游事件序列语义）。
func TestReportEventsPreservesOrderAndPayload(t *testing.T) {
	c, got, done := captureReport(t, `{"code":0,"data":{}}`)
	defer done()

	err := c.ReportEvents(testAuth(), []map[string]any{
		{"eventCode": "e1", "seq": 1.0},
		{"eventCode": "e2", "seq": 2.0, "nested": map[string]any{"k": "v"}},
		{"eventCode": "e3", "seq": 3.0},
	})
	if err != nil {
		t.Fatalf("上报失败：%v", err)
	}
	var arr []map[string]any
	if err := json.Unmarshal(*got, &arr); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(arr) != 3 {
		t.Fatalf("数组长度=%d want 3", len(arr))
	}
	for i, want := range []string{"e1", "e2", "e3"} {
		if arr[i]["eventCode"] != want {
			t.Fatalf("第 %d 条 eventCode=%v want %s", i, arr[i]["eventCode"], want)
		}
	}
	// 嵌套结构必须原样保留（桌面事件族的载荷是多层嵌套的）
	nested, ok := arr[1]["nested"].(map[string]any)
	if !ok || nested["k"] != "v" {
		t.Fatalf("嵌套结构未保留：%+v", arr[1]["nested"])
	}
}

// 空事件列表是**无操作**：不应发起请求。
// 若发成空数组，上游可能记为一次无效上报，浪费每日风控额度。
func TestReportEventsEmptyIsNoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("空事件列表不应发起请求，却请求了 %s", r.URL.Path)
	}))
	defer srv.Close()
	c := New()
	c.BillingBaseCN = srv.URL

	if err := c.ReportEvents(testAuth(), nil); err != nil {
		t.Fatalf("空列表应返回 nil 错误，得 %v", err)
	}
	if err := c.ReportEvents(testAuth(), []map[string]any{}); err != nil {
		t.Fatalf("空切片应返回 nil 错误，得 %v", err)
	}
}

// ReportEventsRaw 直通已序列化载荷（调用方需自行保证是数组）。
func TestReportEventsRawPassesThrough(t *testing.T) {
	c, got, done := captureReport(t, `{"code":0,"data":{}}`)
	defer done()

	body := json.RawMessage(`[{"eventCode":"raw1"}]`)
	if err := c.ReportEventsRaw(testAuth(), body); err != nil {
		t.Fatalf("上报失败：%v", err)
	}
	if string(*got) != string(body) {
		t.Fatalf("载荷被改动：得 %s want %s", string(*got), string(body))
	}
}

// ReportChatActivity 是 ReportEvents 的包装，必须是「单条 + 数组」形态。
func TestReportChatActivityWrapsInArray(t *testing.T) {
	c, got, done := captureReport(t, `{"code":0,"data":{}}`)
	defer done()

	if err := c.ReportChatActivity(testAuth(), "wb2a-u1-123", ""); err != nil {
		t.Fatalf("上报失败：%v", err)
	}
	var arr []map[string]any
	if err := json.Unmarshal(*got, &arr); err != nil {
		t.Fatalf("请求体不是 json 数组：%v（原文 %s）", err, string(*got))
	}
	if len(arr) != 1 {
		t.Fatalf("数组长度=%d want 1", len(arr))
	}
	if arr[0]["eventCode"] != "chat_request_send" {
		t.Fatalf("eventCode=%v want chat_request_send", arr[0]["eventCode"])
	}
	// requestID 为空时应回退为 conversationID
	if arr[0]["requestId"] != "wb2a-u1-123" {
		t.Fatalf("requestId=%v 应回退为 conversationID", arr[0]["requestId"])
	}
}

// 事件构造函数必须带齐上游要求的必备字段。
// 缺字段的表现同样是「200 但不点亮」，必须由测试兜住。
func TestChatRequestSendEventRequiredFields(t *testing.T) {
	a := &auth.Auth{Kind: "workbuddy", UID: "uid-xyz"}
	ev := ChatRequestSendEvent(a, "conv-1", "req-1")

	required := []string{
		"eventCode", "timestamp", "mode", "conversationId", "requestId",
		"inputLength", "requestModelId", "requestModelName",
		"mentionContexts", "knowledgeId", "knowledgeName",
		"presentAt", "rootRequestId", "agentName", "agentType", "userId",
	}
	for _, k := range required {
		if _, ok := ev[k]; !ok {
			t.Errorf("缺少必备字段 %q", k)
		}
	}
	if ev["eventCode"] != "chat_request_send" {
		t.Errorf("eventCode=%v", ev["eventCode"])
	}
	if ev["userId"] != "uid-xyz" {
		t.Errorf("userId=%v want uid-xyz", ev["userId"])
	}
	if ev["requestId"] != "req-1" {
		t.Errorf("requestId=%v want req-1", ev["requestId"])
	}

	// 列表类字段必须是空数组而非 nil —— nil 会被序列化成 null，
	// 与官方客户端的 [] 形态不同。
	for _, k := range []string{"mentionContexts", "knowledgeId", "knowledgeName"} {
		arr, ok := ev[k].([]any)
		if !ok || arr == nil {
			t.Errorf("%s 应为非 nil 空数组，得 %#v", k, ev[k])
		}
	}
}

// requestID 为空时回退为 conversationID（单轮上报场景）。
func TestChatRequestSendEventRequestIDFallback(t *testing.T) {
	ev := ChatRequestSendEvent(&auth.Auth{UID: "u"}, "conv-only", "")
	if ev["requestId"] != "conv-only" {
		t.Fatalf("requestId=%v want conv-only", ev["requestId"])
	}
}
