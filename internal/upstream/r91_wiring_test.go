package upstream

import (
	"net/http"
	"testing"

	"omnigate/internal/auth"
)

// TestChatHeadersSessionStableAcrossTurns 是 R91 的端到端闸门：走真实 ChatHeaders 路径，
// 同一会话的第二轮请求必须复用同一个 X-Conversation-ID，而 X-Conversation-Request-ID
// 与 X-Conversation-Message-ID 必须变。
//
// 这条是「每请求现生成」缺陷的直接反向断言。变异点：把 injectConversationHeaders 改回
// 每次现生成 X-Conversation-ID，本测试必须失败。
func TestChatHeadersSessionStableAcrossTurns(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.codebuddy.cn"}
	round1 := buildChatBody(t, "第一句")
	round2 := buildChatBody(t, "第一句", "第二句")

	req1, _ := http.NewRequest("POST", "https://chat.example/v2/chat/completions", nil)
	c1, tn1 := conversationAnchors(round1)
	ChatHeaders(req1, a, c1, tn1)

	req2, _ := http.NewRequest("POST", "https://chat.example/v2/chat/completions", nil)
	c2, tn2 := conversationAnchors(round2)
	ChatHeaders(req2, a, c2, tn2)

	conv1 := req1.Header.Get("X-Conversation-ID")
	conv2 := req2.Header.Get("X-Conversation-ID")
	if conv1 == "" || conv2 == "" {
		t.Fatalf("X-Conversation-ID 缺失：%q / %q（缺任一 400/11-128，见 R44）", conv1, conv2)
	}
	if conv1 != conv2 {
		t.Fatalf("同一会话的第二轮 X-Conversation-ID 变了：%q -> %q —— 这是 R91 要修的缺陷", conv1, conv2)
	}

	// 轮级：第二轮必须换。
	if r1, r2 := req1.Header.Get("X-Conversation-Request-ID"), req2.Header.Get("X-Conversation-Request-ID"); r1 == r2 {
		t.Fatalf("换轮后 X-Conversation-Request-ID 未变：%q（上游按它计数 user send）", r1)
	}
	// 调用级：每请求必换。
	if m1, m2 := req1.Header.Get("X-Conversation-Message-ID"), req2.Header.Get("X-Conversation-Message-ID"); m1 == m2 {
		t.Fatalf("X-Conversation-Message-ID 未变：%q（每请求必须新值）", m1)
	}
	if m1, x1 := req1.Header.Get("X-Conversation-Message-ID"), req1.Header.Get("X-Request-ID"); m1 != x1 {
		t.Fatalf("X-Request-ID 必须等于 X-Conversation-Message-ID：%q vs %q", x1, m1)
	}
	// X-Root-Request-ID 必须等于本轮的 X-Conversation-Request-ID。
	if root, reqID := req1.Header.Get("X-Root-Request-ID"), req1.Header.Get("X-Conversation-Request-ID"); root != reqID {
		t.Fatalf("X-Root-Request-ID=%q 应等于本轮的 X-Conversation-Request-ID=%q", root, reqID)
	}
	// B3 是每请求链路头，跟调用级 id 走。
	if b3, msg := req1.Header.Get("X-B3-TraceId"), req1.Header.Get("X-Conversation-Message-ID"); b3 != msg {
		t.Fatalf("X-B3-TraceId=%q 应等于调用级 id=%q", b3, msg)
	}
	if span := req1.Header.Get("X-B3-SpanId"); len(span) != 16 {
		t.Fatalf("X-B3-SpanId=%q 应为 16 位", span)
	}
}

// TestChatHeadersRetryReusesTurn 锁「重试复用同一轮 id」：锚点相同 ⇒ 轮级 id 相同。
func TestChatHeadersRetryReusesTurn(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	body := buildChatBody(t, "同一句话")
	conv, turn := conversationAnchors(body)

	var got []string
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest("POST", "https://chat.example/v2/chat/completions", nil)
		ChatHeaders(req, a, conv, turn)
		got = append(got, req.Header.Get("X-Conversation-Request-ID"))
	}
	if got[0] == "" {
		t.Fatalf("X-Conversation-Request-ID 缺失")
	}
	for i := 1; i < len(got); i++ {
		if got[i] != got[0] {
			t.Fatalf("重试必须复用同一轮 id：%q vs %q", got[i], got[0])
		}
	}
}

// TestChatHeadersEmptyAnchorStillSendsAllHeaders 锁兜底路径：锚点为空时仍要发全会话头
// （退回随机，但绝不能缺头 —— 缺头会 400/11-128）。
func TestChatHeadersEmptyAnchorStillSendsAllHeaders(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://chat.example/v2/chat/completions", nil)
	ChatHeaders(req, &auth.Auth{AccessToken: "at", UID: "u1"}, "", "")
	for _, k := range []string{
		"X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID",
		"X-Request-ID", "X-Root-Request-ID",
	} {
		if v := req.Header.Get(k); v == "" {
			t.Errorf("锚点为空时 %s 也必须发（兜底随机）", k)
		}
	}
}
