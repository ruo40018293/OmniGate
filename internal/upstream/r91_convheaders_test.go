package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// buildChatBody 构造 OpenAI 形状的 chat 请求体（user 文本按顺序拼成 messages）。
func buildChatBody(t *testing.T, msgs ...string) []byte {
	t.Helper()
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	out := make([]msg, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, msg{Role: "user", Content: m})
	}
	b, err := json.Marshal(map[string]any{"messages": out})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestConversationAnchorsStableWithinSession 锁「同一会话多轮对话里 conv 恒定」。
//
// 语义（官方 5.8.4 抓包）：X-Conversation-ID 是会话级——同一次对话在第二轮、第三轮
// 追加消息后必须仍是同一个 id。变异点：conversationAnchors 用末条而不是首条 user
// 作为 conv（每轮都变），本测试必须失败。
func TestConversationAnchorsStableWithinSession(t *testing.T) {
	round1 := buildChatBody(t, "第一句：我们在做什么")
	round2 := buildChatBody(t, "第一句：我们在做什么", "第二句：继续说")
	round3 := buildChatBody(t, "第一句：我们在做什么", "第二句：继续说", "第三句：收尾")

	conv1, turn1 := conversationAnchors(round1)
	conv2, turn2 := conversationAnchors(round2)
	conv3, turn3 := conversationAnchors(round3)

	if conv1 == "" || conv2 == "" || conv3 == "" {
		t.Fatalf("锚点不该为空：%q %q %q", conv1, conv2, conv3)
	}
	if conv1 != conv2 || conv2 != conv3 {
		t.Fatalf("会话锚点必须跨轮恒定：%q / %q / %q —— 每轮都变会把一次多轮对话拆成上千个会话",
			conv1, conv2, conv3)
	}
	// turn 是轮级：每轮不同。
	if turn1 == turn2 || turn2 == turn3 {
		t.Fatalf("轮次锚点必须随 user send 变化：%q / %q / %q", turn1, turn2, turn3)
	}
}

// TestTurnAnchorStableForToolCallbacks 锁「一轮内多步复用同一 turn」。
//
// 官方语义：一次 user send 一个 X-Conversation-Request-ID，工具回调/重试复用。
// 工具回调时请求体会追加 assistant/tool 消息，末条 user 仍在 ⇒ turn 不变。
func TestTurnAnchorStableForToolCallbacks(t *testing.T) {
	base := `{"messages":[
		{"role":"user","content":"帮我查天气"},
		{"role":"assistant","content":"","tool_calls":[{"id":"c1"}]}
	]}`
	// 模拟工具结果回填后的第二轮请求（追加 tool 消息，user 消息未变）。
	withTool := `{"messages":[
		{"role":"user","content":"帮我查天气"},
		{"role":"assistant","content":"","tool_calls":[{"id":"c1"}]},
		{"role":"tool","content":"25 度","tool_call_id":"c1"}
	]}`
	c1, t1 := conversationAnchors([]byte(base))
	c2, t2 := conversationAnchors([]byte(withTool))
	if c1 == "" || t1 == "" {
		t.Fatalf("锚点不该为空")
	}
	if c1 != c2 {
		t.Fatalf("会话锚点变了：%q -> %q", c1, c2)
	}
	if t1 != t2 {
		t.Fatalf("工具回调必须复用同一 turn：%q -> %q（上游按 turn id 计数一次 user send）", t1, t2)
	}
}

// TestConversationIDStableAndUidScoped 锁会话 id 的两条性质：同输入恒定、跨账号隔离。
func TestConversationIDStableAndUidScoped(t *testing.T) {
	a := conversationID("uA", "anchor")
	b := conversationID("uA", "anchor")
	if a != b {
		t.Fatalf("同 uid+锚点必须恒定：%q vs %q", a, b)
	}
	c := conversationID("uB", "anchor")
	if a == c {
		t.Fatalf("不同账号同一对话必须不同 id：%q —— 否则上游看到会话跨账号", a)
	}
	// 形态：36 位带连字符的 UUIDv4。
	if len(a) != 36 {
		t.Fatalf("会话 id %q 长度 %d != 36", a, len(a))
	}
	if a[14] != '4' {
		t.Fatalf("会话 id %q version 位应为 4", a)
	}
	switch a[19] {
	case '8', '9', 'a', 'b':
	default:
		t.Fatalf("会话 id %q variant 位应为 8/9/a/b", a)
	}
}

// TestConversationIDRandomWhenNoAnchor 锁兜底：无锚点时退回随机 v4（每次不同）。
func TestConversationIDRandomWhenNoAnchor(t *testing.T) {
	x := conversationID("uA", "")
	y := conversationID("uA", "")
	if x == y {
		t.Fatalf("无锚点时必须每请求随机，两次得到相同 %q", x)
	}
	if len(x) != 36 {
		t.Fatalf("兜底 id 形态 %q 不符", x)
	}
}

// TestTurnIDOfStablePerTurn 锁轮级 id：同 uid+conv+turn 恒定，换 turn 必变。
func TestTurnIDOfStablePerTurn(t *testing.T) {
	a := turnIDOf("u1", "conv-x", "turn-1")
	b := turnIDOf("u1", "conv-x", "turn-1")
	if a != b {
		t.Fatalf("同一轮必须恒定：%q vs %q —— 重试会多计一次 user send", a, b)
	}
	if len(a) != 32 {
		t.Fatalf("轮级 id 必须是 32 位 hex，得到 %q（%d 位）", a, len(a))
	}
	c := turnIDOf("u1", "conv-x", "turn-2")
	if a == c {
		t.Fatalf("换轮必须换 id：%q", a)
	}
}

// TestNewMessageIDIsRandom32Hex 锁调用级 id：32 位 hex、且不重复（crypto/rand）。
//
// 变异点：把 newMessageID 改回时间戳派生，连续两次调用在循环里会撞值（同一纳秒）。
func TestNewMessageIDIsRandom32Hex(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := newMessageID()
		if len(id) != 32 {
			t.Fatalf("调用级 id %q 长度 %d != 32", id, len(id))
		}
		for _, c := range id {
			if !strings.ContainsRune("0123456789abcdef", c) {
				t.Fatalf("调用级 id %q 含非 hex 字符 %c", id, c)
			}
		}
		if seen[id] {
			t.Fatalf("调用级 id 重复：%q —— 熵不足（应是 crypto/rand）", id)
		}
		seen[id] = true
	}
}

// TestConvContentTextHandlesParts 锁多模态 content 分片：只累加 text，图片不参与。
func TestConvContentTextHandlesParts(t *testing.T) {
	s := convContentText(json.RawMessage(`"纯文本"`))
	if s != "纯文本" {
		t.Errorf("裸字符串应原样取出，得到 %q", s)
	}
	parts := convContentText(json.RawMessage(`[
		{"type":"text","text":"前半"},
		{"type":"image_url","image_url":{"url":"data:..."}},
		{"type":"text","text":"后半"}
	]`))
	if parts != "前半后半" {
		t.Errorf("分片应只累加 text，得到 %q", parts)
	}
	if got := convContentText(json.RawMessage(`123`)); got != "" {
		t.Errorf("非法 content 应返回空，得到 %q", got)
	}
	if got := convContentText(nil); got != "" {
		t.Errorf("nil content 应返回空，得到 %q", got)
	}
}

// TestConversationAnchorsRejectsOversizeBody 锁超限退回随机（不做锚点抽取）。
func TestConversationAnchorsRejectsOversizeBody(t *testing.T) {
	big := make([]byte, maxAnchorBody+1)
	for i := range big {
		big[i] = ' '
	}
	if conv, turn := conversationAnchors(big); conv != "" || turn != "" {
		t.Fatalf("超限 body 必须返回空锚点，得到 %q/%q", conv, turn)
	}
	if conv, turn := conversationAnchors(nil); conv != "" || turn != "" {
		t.Fatalf("空 body 必须返回空锚点，得到 %q/%q", conv, turn)
	}
	if conv, turn := conversationAnchors([]byte("not json")); conv != "" || turn != "" {
		t.Fatalf("非法 JSON 必须返回空锚点，得到 %q/%q", conv, turn)
	}
}
