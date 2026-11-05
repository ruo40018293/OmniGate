package gateway

import "testing"

// R94：Anthropic 入站消息里「工具结果 + 新指令」放在同一条 user 消息里是
// Claude Code 的常态（工具执行完、用户又追一句）。转换后出站顺序必须是
// role=tool 在前、新指令在后——反了就会变成 user(文本) → tool → tool，
// 违反「assistant.tool_calls 之后必须紧跟同批 role:tool」，
// 上游实测回 11148 tool calls and tool results do not match。
func TestToolResultPrecedesTrailingUserText(t *testing.T) {
	blocks := []any{
		map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": "命令输出"},
		map[string]any{"type": "text", "text": "再跑一次"},
	}
	out, err := convertAnthropicMessage("user", blocks, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("应产出 2 条消息（tool + user），实际 %d 条：%#v", len(out), out)
	}
	first, _ := out[0].(map[string]any)
	if first["role"] != "tool" {
		t.Fatalf("第一条必须是 role=tool，实际 %#v", first)
	}
	if first["tool_call_id"] != "call_1" {
		t.Fatalf("tool_call_id 必须透传，实际 %#v", first["tool_call_id"])
	}
	second, _ := out[1].(map[string]any)
	if second["role"] != "user" {
		t.Fatalf("最后一条必须是 role=user，实际 %#v", second)
	}
	if second["content"] != "再跑一次" {
		t.Fatalf("新指令内容必须原样保留，实际 %#v", second["content"])
	}
}

// 纯工具结果不得凭空补一条 content 为空的 user（Chat 侧空 user 无意义，
// 且会让上游看到「工具结果之后又跟一条空消息」）。
func TestPureToolResultAddsNoEmptyUser(t *testing.T) {
	blocks := []any{
		map[string]any{"type": "tool_result", "tool_use_id": "call_2", "content": "ok"},
	}
	out, err := convertAnthropicMessage("user", blocks, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("纯工具结果只能产出 1 条 role=tool，实际 %d 条：%#v", len(out), out)
	}
	m, _ := out[0].(map[string]any)
	if m["role"] != "tool" {
		t.Fatalf("唯一一条必须是 role=tool，实际 %#v", m)
	}
}

// assistant 的 tool_use 必须排在工具结果之前（这条行为未变，作为对照闸门）。
func TestAssistantToolCallStaysBeforeToolResults(t *testing.T) {
	blocks := []any{
		map[string]any{"type": "tool_use", "id": "call_3", "name": "Bash", "input": map[string]any{"command": "ls"}},
		map[string]any{"type": "text", "text": "我先看一眼"},
	}
	out, err := convertAnthropicMessage("assistant", blocks, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("应只产出 1 条 assistant 消息，实际 %d 条：%#v", len(out), out)
	}
	m, _ := out[0].(map[string]any)
	if m["role"] != "assistant" {
		t.Fatalf("必须是 role=assistant，实际 %#v", m)
	}
	calls, _ := m["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls 必须保留 1 条，实际 %#v", m["tool_calls"])
	}
	if m["content"] != "我先看一眼" {
		t.Fatalf("文本必须与 tool_calls 并存，实际 %#v", m["content"])
	}
}

// 普通文本 user 消息（无工具）行为不变。
func TestPlainUserTextUnchanged(t *testing.T) {
	out, err := convertAnthropicMessage("user", "你好", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("应产出 1 条，实际 %d 条", len(out))
	}
	m, _ := out[0].(map[string]any)
	if m["role"] != "user" || m["content"] != "你好" {
		t.Fatalf("字符串简写必须原样转换，实际 %#v", m)
	}
}
