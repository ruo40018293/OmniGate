// report.go 对话活跃上报接口：POST {billingBase}/v2/report。
// 一条 chat_request_send 事件同时点亮 growth 连登 + 解锁领养前置任务。
// 不需真实对话，纯事件上报，风控口径每号每天 1 次。
package upstream

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"

	"omnigate/internal/auth"
)

const reportPath = "/v2/report"

// 事件载荷统一用 map[string]any 表达，而非为每种事件定义 struct。
//
// 原因：上游事件类型众多（chat_request_send / chat_message_response /
// agent_task_created / 桌面端事件族 / 小程序事件…），字段随活动期变化。
// 逐个定义 struct 会产生两处定义同一形状的问题（调用方与解析侧），
// 且字段增删需要同步改多处。map 形态让「构造」与「发送」解耦，
// 具体字段口径集中记录在 docs/任务体系协议.md。

// doJSONBilling 发 billing 域请求的通用方法（公之于包内）。
// billingJSON 用 billingBase + BillingHeaders 发送。
func (c *Client) billingReq(a *auth.Auth, method, path string, body json.RawMessage) (json.RawMessage, error) {
	for name, host := range map[string]string{"billingBaseCN": c.BillingBaseCN, "billingBaseGlobal": c.BillingBaseGlob} {
		_ = name
		_ = host
	}
	return c.billingJSON(a, method, path, body)
}

// billingJSON 发 billing 域请求并解信封；body 为 nil 时无请求体。
func (c *Client) billingJSON(a *auth.Auth, method, path string, body json.RawMessage) (json.RawMessage, error) {
	url := c.billingBase(a) + path
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	if len(body) > 0 {
		var rdr = bytes.NewReader(body)
		req, err = http.NewRequest(method, url, rdr)
		if err != nil {
			return nil, err
		}
	}
	BillingHeaders(req, a)
	return c.doJSONBilling(req)
}

// ReportEvents 向上游发送**任意**行为事件（/v2/report）。
//
// 请求体是事件**数组**（不是单个对象）—— 这是上游的既定形态，
// 单条也要用数组包一层。
//
// 事件对象至少要含 `eventCode`；其余字段随事件类型变化，由调用方按需填充。
// 上游对不同任务认不同的事件类型与客户端指纹（见 docs/任务体系协议.md）。
//
// 本方法是底层原语：只负责「把给定载荷发出去」，不做任何事件构造。
// 需要构造具体事件时，用下面的事件构造函数。
func (c *Client) ReportEvents(a *auth.Auth, events []map[string]any) error {
	if len(events) == 0 {
		return nil
	}
	raw, err := json.Marshal(events)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.billingBase(a)+reportPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	BillingHeaders(req, a)
	_, err = c.doJSONBilling(req)
	return err
}

// ReportEventsRaw 发送已序列化的事件数组（供调用方自行控制字段顺序/形状时使用）。
//
// body 必须是 JSON 数组；传入其它形态由上游拒绝。
func (c *Client) ReportEventsRaw(a *auth.Auth, body json.RawMessage) error {
	if len(body) == 0 {
		return nil
	}
	req, err := http.NewRequest(http.MethodPost, c.billingBase(a)+reportPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	BillingHeaders(req, a)
	_, err = c.doJSONBilling(req)
	return err
}

// ReportChatActivity 向上游发送一条对话活跃上报（chat_request_send）。
// conversationID 由调用方生成（如 "wb2a-<uid8>-<ms>"），无需真实会话。
//
// 一条 chat_request_send 同时点亮 growth 连登 + 解锁领养前置任务。
// 风控口径：每号每天 1 次。
//
// 这是 ReportEvents 的便捷包装 —— 保留原签名以免破坏既有调用方。
func (c *Client) ReportChatActivity(a *auth.Auth, conversationID, requestID string) error {
	return c.ReportEvents(a, []map[string]any{
		ChatRequestSendEvent(a, conversationID, requestID),
	})
}

// ChatRequestSendEventModel 构造一条**对齐指定模型**的 chat_request_send 事件。
//
// 用途：部分任务按「用过哪个模型」判定（如 Model_chat_GLM5.2、black_cat 夜猫子），
// 此时 requestModelId / requestModelName 必须与任务要求的模型一致 ——
// 用默认模型上报不会计入该任务进度。
func ChatRequestSendEventModel(a *auth.Auth, conversationID, requestID, modelID, modelName string) map[string]any {
	ev := ChatRequestSendEvent(a, conversationID, requestID)
	ev["requestModelId"] = modelID
	ev["requestModelName"] = modelName
	return ev
}

// ChatRequestSendEvent 构造一条 chat_request_send 事件（与官方桌面端同构）。
//
// 字段口径来自实测抓包；`mode` 固定 "craft"，耗时字段按官方默认填。
// requestID 为空时回退为 conversationID（单轮场景）。
//
// ⚠️ userId 必填：缺失时上游返回 200 但**静默丢弃**事件 —— 表现为
// 「上报成功却不涨进度」，极难定位。
func ChatRequestSendEvent(a *auth.Auth, conversationID, requestID string) map[string]any {
	if requestID == "" {
		requestID = conversationID
	}
	now := time.Now().UnixMilli()
	return map[string]any{
		"eventCode":             "chat_request_send",
		"timestamp":             now,
		"mode":                  "craft",
		"conversationId":        conversationID,
		"requestId":             requestID,
		"inputLength":           12,
		"requestModelId":        "deepseek-v4-flash",
		"requestModelName":      "DeepSeek V4 Flash",
		"isPlan":                false,
		"isAutoExecuteTerminal": false,
		"isAutoModify":          false,
		"codebaseEnable":        false,
		"maxToken":              0,
		"temperature":           0,
		"mentionContexts":       []any{},
		"knowledgeId":           []any{},
		"knowledgeName":         []any{},
		"command":               "",
		"presentAt":             now,
		"rootRequestId":         conversationID,
		"agentName":             "default",
		"agentType":             "conversation",
		"userId":                a.UID,
	}
}
