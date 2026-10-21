// desktop.go 桌面客户端（WorkBuddy Desktop）行为指纹上报。
//
// ## 为什么要「伪造客户端指纹」
//
// 成长任务里有一批被上游标记为「需电脑端」「桌面端专属」，它们的判据不是
// accept 状态，而是**同一条 /v2/report 通道上的客户端指纹**：
//
//	POST https://copilot.tencent.com/v2/report     ← chatBase（CLI 上报走 billingBase）
//	User-Agent: WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1
//	X-Domain: copilot.tencent.com, X-Product: SaaS, X-User-Id: <uid>
//	Body: [ {...event...} ]                        ← 数组
//
// 每个事件除业务字段外必带桌面指纹（ideName=WorkBuddy、extName=workbuddy-desktop 等）。
// 换一套指纹（web / 小程序）就是另一批任务的判据 —— 见 mp_report.go。
//
// ## 来源与可信度
//
// 事件形状来自社区既有公开成果（第三方项目的抓包与客户端包逆向，2026-09 样本），
// **本项目未独立实测**。
// 字段名写错时上游通常仍返回 200（只是不入账），因此调用方必须靠**回读进度**
// 判断是否真的点亮，不能只看 HTTP 状态码。
//
// ## 域划分（易错）
//
//	desktop 域（copilot.tencent.com）  RichMeow / Buddy_App / 模板 / 灵感 / 专家 / 画布
//	web     域（www.workbuddy.cn）      Library_read 等浏览器行为类
//	mp      域（www.codebuddy.cn）      小程序口径（见 mp_report.go）
package upstream

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"omnigate/internal/auth"
)

const (
	desktopReportPath    = "/v2/report"
	desktopAppearanceSet = "/v2/user-asset/appearance/set"
	// desktopUA 桌面客户端 UA（5.5.6 内嵌 CLI 2.137.1）。
	desktopUA = "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1"
	// webUA 浏览器 UA（web 域行为类任务用）。
	webUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
)

// deriveFingerprintID 由 uid 稳定派生 36 位 hex 设备标识（machineId/sessionId 复用）。
//
// 幂等：同一账号每次生成相同值，模拟「固定设备」——
// 每次随机会让上游看到设备频繁变化，反而更像异常客户端。
func deriveFingerprintID(a *auth.Auth, salt string) string {
	uid := ""
	if a != nil {
		uid = a.UID
	}
	sum := sha256.Sum256([]byte(salt + ":" + uid))
	return hex.EncodeToString(sum[:18]) // 36 hex chars
}

// desktopFingerprint 桌面端公共指纹（注入每个事件，业务字段可覆盖）。
func desktopFingerprint(a *auth.Auth) map[string]any {
	now := time.Now().UnixMilli()
	nick := ""
	if a != nil {
		nick = a.Nickname
	}
	return map[string]any{
		"timezone":     "Asia/Shanghai",
		"reportDelay":  2000,
		"userId":       a.UID,
		"username":     nick,
		"userNickname": nick,
		"product":      "SaaS",
		"releaseDate":  int64(1789036585355),
		"commit":       "5f9692923c93033111c51ad7b003eb80204a9b75",
		"ideName":      "WorkBuddy",
		"ideType":      "WorkBuddy",
		"ideVersion":   "5.5.6",
		"machineId":    deriveFingerprintID(a, "machine"),
		"sessionId":    deriveFingerprintID(a, "session"),
		"extName":      "workbuddy-desktop",
		"extVersion":   "5.5.6",
		"os":           "win32",
		"arch":         "x64",
		"osVersion":    "10.0.26220",
		"cpuCores":     20,
		"memorySize":   24,
		"timestamp":    now,
		"presentAt":    now,
	}
}

// ReportDesktopEvent 以桌面客户端指纹向 chatBase 的 /v2/report 批量上报事件。
//
// 请求体是事件**数组**（单条也要包一层）；公共指纹自动注入，业务字段优先。
// 返回 nil **不代表任务已点亮** —— 上游对不入账的事件同样返回 200，
// 判定必须靠回读任务进度。
func (c *Client) ReportDesktopEvent(a *auth.Auth, events ...map[string]any) error {
	if len(events) == 0 {
		return fmt.Errorf("desktop report: no events")
	}
	fp := desktopFingerprint(a)
	arr := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		m := make(map[string]any, len(fp)+len(ev))
		for k, v := range fp {
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
	req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+desktopReportPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("User-Agent", desktopUA)
	req.Header.Set("X-Domain", c.chatBase(a))
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Request-ID", deriveFingerprintID(a, "req")+fmt.Sprintf("%d", time.Now().UnixNano()%1e6))
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	_, err = c.doJSON(req)
	return err
}

// ReportWebEvent 以 Web 端指纹向 webBase 的 /v2/report 上报单事件。
//
// 与桌面指纹不同：web 域事件是浏览器形状（os/machineId/userAgent），
// 用于 Library_read 这类页面行为任务。
func (c *Client) ReportWebEvent(a *auth.Auth, eventCode, pageURL, elementID, elementName string) error {
	ev := map[string]any{
		"eventCode": eventCode, "timestamp": time.Now().UnixMilli(), "reportDelay": 0,
		"pageURL": pageURL, "elementId": elementID, "elementName": elementName,
		"os": "Win32", "arch": "", "osVersion": "10.0", "userAgent": webUA,
		"machineId": deriveFingerprintID(a, "webmachine"), "userId": a.UID,
		"userNickname": a.Nickname, "enterpriseId": a.EnterpriseID,
	}
	raw, err := json.Marshal([]map[string]any{ev})
	if err != nil {
		return err
	}
	base := c.webBase(a)
	req, err := http.NewRequest(http.MethodPost, base+"/v2/report", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-client-platform", "web")
	req.Header.Set("Origin", base)
	req.Header.Set("Referer", pageURL)
	req.Header.Set("User-Agent", webUA)
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	_, err = c.doJSON(req)
	return err
}

// ---------------------------------------------------------------------------
// 事件链构造（纯函数，不发请求 —— 便于单测）
// ---------------------------------------------------------------------------

// DesktopChatSequence 构造一次「桌面端成功对话」的完整事件链。
//
// agent_task_created → chat_message_send → chat_request_send →
// chat_message_response(isSuccessful=true) → chat_message_status → chat_request_response。
// 该链用于 RichMeow_Chat（桌面端对话 1 次）。
func DesktopChatSequence(conversationID, requestID, messageID, modelID, modelName string) []map[string]any {
	mk := func(code string, extra map[string]any) map[string]any {
		ev := map[string]any{"eventCode": code}
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}
	now := time.Now().UnixMilli()
	return []map[string]any{
		mk("agent_task_created", map[string]any{
			"source": "LOCAL", "name": "working", "task_target": "local", "mode": "craft",
			"requestModelId": modelID, "requestModelName": modelName,
			"has_repo": false, "repo_type": "none", "workspace_type": "empty",
			"has_connector": false, "connector_types": []any{},
			"has_mention": false, "mention_types": []any{},
			"has_template": false, "action": "", "template_name": "",
			"has_expert": false, "expert_id": "", "expert_name": "", "expert_industry_id": "",
			"has_skill": false, "skill_names": []any{},
			"conversationId": conversationID, "messageId": messageID,
			"buddyId": "", "buddyName": "",
		}),
		mk("chat_message_send", map[string]any{
			"messageId": messageID + "-assistant", "historyCount": 0,
			"isContextTruncated": false, "currentStepCount": 1,
			"traceId": requestID, "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
		}),
		mk("chat_request_send", map[string]any{
			"inputLength": 24, "isPlan": false, "isAutoExecuteTerminal": false,
			"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
			"maxSteps": 500, "temperature": 0, "maxRetries": 0,
			"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
			"codebaseId": "", "mentionContextCount": 0, "command": "",
			"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
			"traceId": requestID, "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
			"codebuddy.session_id":              conversationID,
			"codebuddy.conversation_request_id": requestID,
		}),
		mk("chat_message_response", map[string]any{
			"messageId": messageID + "-assistant", "responseModelId": modelID,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"firstTokenAt": now, "traceId": requestID,
			"conversationId": conversationID,
			"rootRequestId":  requestID, "parentConversationId": conversationID,
			"agentName": "cli", "agentType": "main",
			"codebuddy.session_id":              conversationID,
			"codebuddy.conversation_request_id": requestID,
		}),
		mk("chat_message_status", map[string]any{
			"messageId": messageID + "-assistant", "messageErrorCode": "0",
			"traceId": requestID, "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
		}),
		mk("chat_request_response", map[string]any{
			"mode": "craft", "toolCallCount": 0,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"rootRequestId": requestID, "parentConversationId": conversationID,
		}),
	}
}

// DesktopBuddyAppSequence 「进入 Buddy 应用」五连事件。
//
// 同一组事件同时满足 Buddy_App（进入任一应用）与 Buddy_App_QQ（判据应用为
// 企鹅教师助手）—— 故默认 buddyID 用后者。
func DesktopBuddyAppSequence(buddyID, buddyName string) []map[string]any {
	mk := func(code string, extra map[string]any) map[string]any {
		ev := map[string]any{
			"eventCode": code, "mode": "LOCAL",
			"buddyId": buddyID, "buddyName": buddyName,
		}
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}
	return []map[string]any{
		mk("buddyapp_discover_click", nil),
		mk("buddyapp_show", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2}),
		mk("buddyapp_enter_click", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2, "isFirstPage": "1"}),
		mk("buddyapp_auth_confirm_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
		mk("buddyapp_bindaccount_skip_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
	}
}

// DesktopBuddyAppQQID / DesktopBuddyAppQQName 企鹅教师助手（Buddy_App_QQ 判据应用）。
const (
	DesktopBuddyAppQQID   = "cb_y5Dy46tPQGGWtueMxXbe"
	DesktopBuddyAppQQName = "企鹅教师助手"
)

// DesktopAutomationCreateEvent 「定时任务创建成功」事件（automation_1 判据）。
func DesktopAutomationCreateEvent(name string) map[string]any {
	return map[string]any{
		"eventCode": "automated_task_create_suc", "name": name,
		"source": "manually", "modelId": "fast-model", "modelIsThinking": true,
		"connectorCount": 0, "skills": "", "skillCount": 0,
		"scheduleType": "once", "mode": "LOCAL",
	}
}

// DesktopTemplateUseSequence 「使用模板创建任务」事件组（template_5 判据）：
// 完整 chat 链 + agent_task_created_with_template + template_used。
// 每组代表「用某个模板创建了一次任务」，5 组即 5/5。
func DesktopTemplateUseSequence(conversationID, requestID, templateID, templateName string) []map[string]any {
	events := DesktopChatSequence(conversationID, requestID, "msg-"+templateID, "fast-model", "fast-model")
	return append(events,
		map[string]any{
			"eventCode": "agent_task_created_with_template", "mode": "working",
			"isCustomModel": false, "id": templateID, "name": templateName, "requestId": requestID,
		},
		map[string]any{"eventCode": "template_used", "template_id": templateID, "task_mode": "working"},
	)
}

// DesktopPlaybookPromptSequence 「灵感案例做同款」事件组（playbook_prompt 判据）：
// web_element_click(playbook_ctaClick) + playbook_cta_click + playbook_prompt_send，
// 后两者带 case 载荷且 JOIN chat 链。
func DesktopPlaybookPromptSequence(conversationID, requestID, caseID, caseName string) []map[string]any {
	events := DesktopChatSequence(conversationID, requestID, "msg-pb", "fast-model", "fast-model")
	payload := map[string]any{
		"id": caseID, "name": caseName, "type": "document",
		"categoryId": "", "categoryName": "",
	}
	withPayload := func(base map[string]any) map[string]any {
		for k, v := range payload {
			base[k] = v
		}
		return base
	}
	return append(events,
		map[string]any{
			"eventCode": "web_element_click", "pageName": "playbook_detail",
			"elementId": "playbook_ctaClick", "elementName": caseName, "source": "discover",
		},
		withPayload(map[string]any{"eventCode": "playbook_cta_click", "source": "discover", "position": 0}),
		withPayload(map[string]any{
			"eventCode": "playbook_prompt_send", "conversationId": conversationID, "requestId": requestID,
		}),
	)
}

// DesktopDesignCanvasSequence 「设计创意画布」事件组（create_canvas 判据）。
func DesktopDesignCanvasSequence(conversationID, requestID string) []map[string]any {
	events := DesktopChatSequence(conversationID, requestID, "msg-canvas", "fast-model", "fast-model")
	tail := requestID[len(requestID)-8:]
	return append(events,
		map[string]any{
			"eventCode": "wbx_design_canvas_task_create", "conversationId": conversationID,
			"requestId": requestID, "source": "summon_keyword", "cost": 12000, "isSuccessful": true,
		},
		map[string]any{
			"eventCode": "wbx_design_canvas_open", "conversationId": conversationID,
			"requestId": requestID, "id": "ardot-file-" + tail,
			"source": "summon_keyword", "type": "page", "cost": 13000, "isSuccessful": true,
		},
	)
}

// DesktopSkillInfoEvent 「技能加载」事件（skill_1 判据）。
// 需 JOIN 一次**真实对话**的 conversationId / requestId（自造 id 不入账）。
func DesktopSkillInfoEvent(conv, req, msgID, modelID, modelName string) map[string]any {
	return map[string]any{
		"eventCode":      "skill_info",
		"id":             "润泽小馆·日报撰写",
		"skillId":        "skill_2097350077599879168",
		"skillVersion":   "1.0.0",
		"toolStatus":     "success",
		"fileCount":      56,
		"source":         "workbuddy-desktop",
		"conversationId": conv, "requestId": req, "messageId": msgID,
		"requestModelId": modelID, "requestModelName": modelName,
		"traceId": req,
	}
}

// SetAppearanceTheme 应用外观主题（Hp_Appearance 的「留痕」半步）。
//
// 判据是随后的 appearance_skin_apply 事件；本调用只负责让服务端有切换记录。
func (c *Client) SetAppearanceTheme(a *auth.Auth, resourceKey string) error {
	raw, err := json.Marshal(map[string]string{"kind": "theme", "resource_key": resourceKey})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+desktopAppearanceSet, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("User-Agent", desktopUA)
	req.Header.Set("X-Product", "SaaS")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	_, err = c.doJSON(req)
	return err
}

// DesktopSkinApplyEvent 「皮肤生效」事件（Hp_Appearance 的真正判据）。
func DesktopSkinApplyEvent(themeKey string) map[string]any {
	return map[string]any{
		"eventCode": "appearance_skin_apply", "action": "apply", "source": "settings_close",
		"id": themeKey, "vipLevel": 0, "series": "", "type": "unknown",
	}
}

// HpAppearanceThemeKey Hp_Appearance 判据主题（和平精英激战金秋）。
const HpAppearanceThemeKey = "theme-tkmw7j"

// ---------------------------------------------------------------------------
// 专家市场（expert_5 / Expert_team_use_3 / Expert_lighthouse / Sequential_Tasks_2）
//
// expert_actual_use 的判据要求 expert_id 是平台上**真实存在**的专家，
// 且 requestId 必须是**真实对话的服务端 id**（自造 UUID 不入账）。
// ---------------------------------------------------------------------------

// MarketExpert 专家市场条目（响应子集）。
type MarketExpert struct {
	ExpertID      string `json:"expert_id"`
	ExpertType    string `json:"expert_type"`
	DisplayNameZH string `json:"display_name_zh"`
	ProfessionZH  string `json:"profession_zh"`
	Version       string `json:"version"`
	Categories    []any  `json:"categories"`
}

// DisplayName 返回可用于事件载荷的显示名（中文名缺失时回退职业名）。
func (e MarketExpert) DisplayName() string {
	if e.DisplayNameZH != "" {
		return e.DisplayNameZH
	}
	return e.ProfessionZH
}

// categoryOf 取首个分类（缺省 "expert-all"）。
func (e MarketExpert) categoryOf() string {
	if len(e.Categories) > 0 {
		if s, ok := e.Categories[0].(string); ok && s != "" {
			return s
		}
	}
	return "expert-all"
}

// versionOr 版本号（缺省 1.0.0）。
func (e MarketExpert) versionOr() string {
	if e.Version != "" {
		return e.Version
	}
	return "1.0.0"
}

// MarketExpertList 拉取专家市场真实专家列表（expertType: "agent" 单专家 / "team" 专家团）。
func (c *Client) MarketExpertList(a *auth.Auth, expertType string) ([]MarketExpert, error) {
	body := map[string]any{"page": 1, "page_size": 20, "sort_by": "reco_rank", "sort_order": "desc"}
	if expertType != "" {
		body["expert_type"] = expertType
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost,
		c.chatBase(a)+"/portal/operation-platform/market/expert/list", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", desktopUA)
	req.Header.Set("X-Domain", c.chatBase(a))
	req.Header.Set("X-Product", "SaaS")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var out struct {
		Experts []MarketExpert `json:"experts"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("expert list parse: %w", err)
	}
	return out.Experts, nil
}

// DesktopExpertSummonSequence 「召唤平台专家」事件组。
func DesktopExpertSummonSequence(e MarketExpert) []map[string]any {
	return []map[string]any{
		{
			"eventCode": "web_element_click", "source": e.ExpertID, "type": e.categoryOf(),
			"version":   e.versionOr(),
			"elementId": "expert_summon_click", "elementName": "立即召唤",
			"pageURL": "/C:/Program%20Files/WorkBuddy/resources/app.asar/renderer/index.html",
		},
		{
			"eventCode": "expert_summon_click", "id": e.ExpertID, "name": e.DisplayName(),
			"expertTitle": e.ProfessionZH, "type": "expert-all", "position": 0,
			"expertType": e.ExpertType, "version": e.versionOr(), "mode": "LOCAL",
		},
		{
			"eventCode": "expert_summoned", "id": e.ExpertID, "name": e.DisplayName(),
			"expertTitle": e.ProfessionZH, "type": "expert-all",
		},
	}
}

// DesktopExpertActualUseEvent 「专家真实使用」事件（expert_5 / Expert_team_use_3 判据）。
// mode 由 modeArg 指定："craft"（普通）或 "LOCAL"（Expert_lighthouse 要求）。
func DesktopExpertActualUseEvent(e MarketExpert, conv, req, modeArg string) map[string]any {
	tail := req
	if len(tail) > 8 {
		tail = tail[len(tail)-8:]
	}
	return map[string]any{
		"eventCode": "expert_actual_use",
		"id":        e.ExpertID, "name": e.DisplayName(), "expertTitle": e.ProfessionZH,
		"type": e.categoryOf(), "expertType": e.ExpertType, "source": "builtin",
		"version": e.versionOr(), "cost": 9000, "characterCount": 14, "mode": modeArg,
		"conversationId": conv, "requestId": req, "messageId": "msg-" + tail,
		"requestModelId": "fast-model", "requestModelName": "fast-model",
	}
}

// serverRequestIDRe 服务端 requestId 形状（cmb- 前缀 32hex，或裸 32hex）。
var serverRequestIDRe = regexp.MustCompile(`^(cmb-)?[0-9a-f]{32}$`)

// DesktopChatWithExpert 发一条**真实**桌面指纹对话（可带 X-Expert-Id），
// 并从 SSE 流里解析服务端返回的 requestId（data.id）。
//
// expert_actual_use 等 JOIN 事件的 requestId 必须是该服务端 id ——
// 自造 UUID 不上账。返回 (conversationID, requestID, error)。
func (c *Client) DesktopChatWithExpert(a *auth.Auth, expertID string) (string, string, error) {
	conversationID := fmt.Sprintf("wb2api-conv-%d", time.Now().UnixNano())
	body, err := json.Marshal(map[string]any{
		"model": "fast-model",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant. 当前处于中文环境，使用简体中文回答。"},
			map[string]any{"role": "user", "content": "1+1等于几？直接回答。"},
		},
		"agent":          "cli",
		"temperature":    1,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	})
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+"/v2/chat/completions",
		bytes.NewReader(PrepareBody(body)))
	if err != nil {
		return "", "", err
	}
	h := req.Header
	h.Set("Authorization", "Bearer "+a.AccessTokenValue())
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	h.Set("User-Agent", desktopUA)
	h.Set("X-Domain", c.chatBase(a))
	h.Set("X-Product", "SaaS")
	h.Set("X-User-Id", a.UID)
	h.Set("X-Conversation-ID", conversationID)
	h.Set("X-Request-ID", fmt.Sprintf("%d", time.Now().UnixNano()))
	h.Set("X-Agent-Intent", "craft")
	h.Set("X-Agent-Type", "main")
	h.Set("X-IDE-Name", "WorkBuddy")
	h.Set("X-IDE-Type", "WorkBuddy")
	h.Set("X-IDE-Version", "5.5.6")
	h.Set("x-codebuddy-request", "1")
	if expertID != "" {
		h.Set("X-Expert-Id", expertID)
	}

	hc := c.StreamHTTP
	if hc == nil {
		hc = c.HTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return "", "", fmt.Errorf("chat http %d: %s", resp.StatusCode, truncate(string(raw), 160))
	}
	// 边读边扫 `"id":"<hex32>"`。必须推进 searchFrom ——
	// 否则首个不匹配的 id 会让循环永远命中同一位置，读满上限后误报「未找到」。
	var buf []byte
	tmp := make([]byte, 8192)
	searchFrom := 0
	const capLimit = 1 << 20
	for len(buf) < capLimit {
		n, rerr := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if i := bytes.Index(buf[searchFrom:], []byte(`"id":"`)); i >= 0 {
				abs := searchFrom + i
				rest := buf[abs+6:]
				if end := bytes.IndexByte(rest, '"'); end > 0 {
					id := string(rest[:end])
					if serverRequestIDRe.MatchString(id) {
						return conversationID, id, nil
					}
					searchFrom = abs + 1
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	return "", "", fmt.Errorf("SSE 中未找到服务端 requestId")
}

// ---------------------------------------------------------------------------
// 便捷布尔判定
// ---------------------------------------------------------------------------

// IsDesktopTaskCode 报告任务是否走「桌面/Web 事件链」而不是 chat 域行为上报。
// 供编排层选择动作时参考（当前未用于分支，仅作文档性导出）。
func IsDesktopTaskCode(code string) bool {
	switch strings.TrimSpace(code) {
	case "RichMeow_Chat", "Buddy_App", "Buddy_App_QQ", "automation_1",
		"Library_read", "template_5", "playbook_prompt", "create_canvas",
		"Hp_Appearance", "skill_1", "expert_5", "Expert_team_use_3", "Expert_lighthouse":
		return true
	default:
		return false
	}
}
