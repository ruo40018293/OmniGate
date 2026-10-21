// mp_report.go 小程序（mini program）口径的指纹上报与事件构造。
//
// 与 desktop.go 同思路但**换一套指纹**：小程序埋点的 extName=workbuddy-mp、
// platform=mini_program，走 billingBase（www.codebuddy.cn）的 /v2/report。
//
// 小程序口径任务（school_season / Sequential_Tasks_*）在默认任务列表里**不出现**，
// 列表、accept、claim 全链路都要求 X-Client-Platform: miniprogram（见 growth.go）。
//
// ## 反作弊（重要）
//
// Sequential_Tasks_3「完成 5 次有效对话」对**连发**有反作弊：数秒级连发的事件会
// 先计入进度（回读甚至 5/5），随后被整体回滚（claim 返回 400 task not completed）。
// 第三方指出的存活间隔是 45s（见 growthTaskMPChatGap）。因此调用方宁可慢也不能
// 白报 —— 这也是本任务集在面板上默认关闭的原因之一。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"omnigate/internal/auth"
)

// schoolOpenDayActivityID 校园日 / 开学季活动 id（事件 activityId 字段值）。
const schoolOpenDayActivityID = "school_open_day_2026"

// mpEventBase 小程序埋点公共指纹。
func mpEventBase(a *auth.Auth) map[string]any {
	nick := ""
	if a != nil {
		nick = a.Nickname
	}
	return map[string]any{
		"timestamp":    time.Now().UnixMilli(),
		"ideType":      "WorkBuddy_MP",
		"ideVersion":   "2.4.0",
		"extName":      "workbuddy-mp",
		"extVersion":   "2.4.0",
		"product":      "SaaS",
		"ideName":      "wx_app_cloud",
		"platform":     "mini_program",
		"os":           "windows",
		"osVersion":    "11",
		"arch":         "x64",
		"machineId":    "0655736a-607f-4d9d-b430-58176ee9a090",
		"timezone":     "Asia/Shanghai",
		"userId":       a.UID,
		"userNickname": nick,
	}
}

// ReportMPEvent 以小程序指纹向 billingBase 的 /v2/report 批量上报事件。
func (c *Client) ReportMPEvent(a *auth.Auth, events ...map[string]any) error {
	if len(events) == 0 {
		return fmt.Errorf("mp report: no events")
	}
	base := mpEventBase(a)
	arr := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		m := make(map[string]any, len(base)+len(ev))
		for k, v := range base {
			m[k] = v
		}
		for k, v := range ev {
			m[k] = v
		}
		arr = append(arr, m)
	}
	raw, err := json.Marshal(arr)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.billingBase(a)+"/v2/report", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	req.Header.Set("X-Client-Product", "workbuddy-mp")
	req.Header.Set("X-Client-Version", "2.4.0")
	req.Header.Set("X-Client-Platform", "mp-weixin")
	req.Header.Set("X-Platform", "wechatmp")
	_, err = c.doJSON(req)
	return err
}

// MpChatEvent 构造一条小程序 chat_request_send 事件（Sequential_Tasks_1/_3 判据）。
//
// activityID 非空时追加 activityId 字段（school_season 必须带，否则不点亮；
// Sequential_Tasks_1/_3 不带）。
func MpChatEvent(conversationID, activityID string) map[string]any {
	rid := "wb2api-" + clientToken()
	tail := rid
	if len(tail) > 8 {
		tail = tail[len(tail)-8:]
	}
	ev := map[string]any{
		"eventCode":   "chat_request_send",
		"inputLength": 14, "isPlan": false, "isAutoExecuteTerminal": false,
		"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
		"maxSteps": 500, "temperature": 0, "maxRetries": 0,
		"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
		"codebaseId": "", "mentionContextCount": 0, "command": "",
		"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
		"traceId": rid, "rootRequestId": rid,
		"parentConversationId": conversationID, "conversationId": conversationID,
		"messageId": "msg-" + tail,
		"agentName": "mp", "agentType": "main",
		"codebuddy.session_id":              conversationID,
		"codebuddy.conversation_request_id": rid,
	}
	if activityID != "" {
		ev["activityId"] = activityID
	}
	return ev
}

// MpChatEventWithModel 小程序对话事件 + 模型字段（Sequential_Tasks_5 判据载体）。
func MpChatEventWithModel(conversationID, modelID, modelName string) map[string]any {
	ev := MpChatEvent(conversationID, "")
	ev["requestModelId"] = modelID
	ev["requestModelName"] = modelName
	return ev
}

// MpExpertUseEvent 小程序口径 expert_actual_use（Sequential_Tasks_2 判据）。
//
// 与桌面口径是**两套**，勿混：不带 conversationId/activityId，
// extVersion 为小程序版本 2.2.8，type 固定 "send_message"。
// expertID 必须是市场真实 ex_ id（空 id 不入账）。
func MpExpertUseEvent(expertID, expertName, expertType string) map[string]any {
	if expertType == "" {
		expertType = "agent"
	}
	if expertName == "" {
		expertName = expertID
	}
	return map[string]any{
		"eventCode": "expert_actual_use", "reportDelay": 0,
		"extVersion": "2.2.8", "source": "mini_program",
		"id": expertID, "name": expertID,
		"expertTitle": expertName, "type": "send_message",
		"characterCount": 12, "expertType": expertType,
	}
}

// MpPlaybookEvents 小程序指纹灵感事件组（Sequential_Tasks_7 备选判据）。
func MpPlaybookEvents(caseID, caseName string) []map[string]any {
	base := map[string]any{
		"id": caseID, "name": caseName, "type": "document",
		"categoryId": "", "categoryName": "",
		"skills": "", "skillNames": "",
	}
	mk := func(extra map[string]any) map[string]any {
		m := make(map[string]any, len(base)+len(extra))
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	return []map[string]any{
		mk(map[string]any{
			"eventCode": "playbook_cta_click", "source": "discover", "position": 1,
			"extVersion": "2.2.8",
		}),
		mk(map[string]any{
			"eventCode": "playbook_prompt_send", "source": "discover",
			"promptLength": 96, "isOfficial": 1,
			"conversationId": "wb2api-mp-pb-" + clientToken(),
			"extVersion":     "2.2.8",
		}),
	}
}
