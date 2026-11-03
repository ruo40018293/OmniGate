package workbuddyai

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"omnigate/internal/auth"
)

func wbaiBuildBody(t *testing.T, msgs ...string) []byte {
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

// TestWBAISessionStableAcrossTurns 是 R91 在国际版一侧的闸门：同一会话第二轮必须复用
// 同一个 X-Conversation-ID。
//
// R44 已证的契约（不许放宽）：X-Conversation-ID 是必填项，缺它一律 400 code=11-128；
// 但**取值语义**（会话级恒定）此前从未被约束——五个头每请求全同一个现生成 mid。
// 变异点：WBAI 的 chatHeaders 改回「五头同一个 mid」，本测试必须失败。
func TestWBAISessionStableAcrossTurns(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	round1 := wbaiBuildBody(t, "第一句")
	round2 := wbaiBuildBody(t, "第一句", "第二句")

	req1, _ := http.NewRequest("POST", "https://chat.example/v2/chat/completions", nil)
	c1, tn1 := conversationAnchors(round1)
	chatHeaders(req1, a, c1, tn1)

	req2, _ := http.NewRequest("POST", "https://chat.example/v2/chat/completions", nil)
	c2, tn2 := conversationAnchors(round2)
	chatHeaders(req2, a, c2, tn2)

	conv1 := req1.Header.Get("X-Conversation-ID")
	conv2 := req2.Header.Get("X-Conversation-ID")
	if conv1 == "" {
		t.Fatalf("X-Conversation-ID 缺失：上游必回 400 code=11-128（R44）")
	}
	if conv1 != conv2 {
		t.Fatalf("同一会话第二轮 X-Conversation-ID 变了：%q -> %q", conv1, conv2)
	}
	// 轮级换、调用级换。
	if r1, r2 := req1.Header.Get("X-Conversation-Request-ID"), req2.Header.Get("X-Conversation-Request-ID"); r1 == r2 {
		t.Fatalf("换轮后 X-Conversation-Request-ID 未变：%q", r1)
	}
	if m1, m2 := req1.Header.Get("X-Conversation-Message-ID"), req2.Header.Get("X-Conversation-Message-ID"); m1 == m2 {
		t.Fatalf("X-Conversation-Message-ID 未变：%q", m1)
	}
	if m1, x1 := req1.Header.Get("X-Conversation-Message-ID"), req1.Header.Get("X-Request-ID"); m1 != x1 {
		t.Fatalf("X-Request-ID=%q 应等于 X-Conversation-Message-ID=%q", x1, m1)
	}
	if root, reqID := req1.Header.Get("X-Root-Request-ID"), req1.Header.Get("X-Conversation-Request-ID"); root != reqID {
		t.Fatalf("X-Root-Request-ID=%q 应等于本轮 X-Conversation-Request-ID=%q", root, reqID)
	}
}

// TestWBAIHeadersNeverIdenticalAcrossTurns 锁「三头不再同值」这一旧缺陷的具体形态。
// R91 之前 X-Conversation-ID / Request-ID / Message-ID 三者恒等（同一个 mid），
// 而那正是把一次多轮对话拆成上千个一次性会话的直接原因。
func TestWBAIHeadersNeverIdenticalAcrossTurns(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://chat.example/v2/chat/completions", nil)
	conv, turn := conversationAnchors(wbaiBuildBody(t, "一句话"))
	chatHeaders(req, &auth.Auth{AccessToken: "at", UID: "u1"}, conv, turn)

	id := req.Header.Get("X-Conversation-ID")
	rid := req.Header.Get("X-Conversation-Request-ID")
	mid := req.Header.Get("X-Conversation-Message-ID")
	if id == rid || id == mid || rid == mid {
		t.Fatalf("三个会话头的语义不同，取值不应相同：conv=%q req=%q msg=%q", id, rid, mid)
	}
	if len(id) != 36 {
		t.Errorf("会话级 id %q 应为带连字符 UUID（36 位）", id)
	}
	if len(rid) != 32 || len(mid) != 32 {
		t.Errorf("轮级/调用级 id 应为 32 位 hex：req=%q msg=%q", rid, mid)
	}
}

// TestWBAISessionScopedByUid 锁国际版侧同样按账号隔离会话。
func TestWBAISessionScopedByUid(t *testing.T) {
	conv, turn := conversationAnchors(wbaiBuildBody(t, "同一句话"))
	ra, _ := http.NewRequest("POST", "https://chat.example/v2/chat/completions", nil)
	chatHeaders(ra, &auth.Auth{AccessToken: "at", UID: "uA"}, conv, turn)
	rb, _ := http.NewRequest("POST", "https://chat.example/v2/chat/completions", nil)
	chatHeaders(rb, &auth.Auth{AccessToken: "at", UID: "uB"}, conv, turn)
	if ra.Header.Get("X-Conversation-ID") == rb.Header.Get("X-Conversation-ID") {
		t.Fatalf("不同账号同一对话必须不同 X-Conversation-ID")
	}
}

// TestWBAINewMessageIDIsRandom 锁国际版调用级 id 用 crypto/rand（不是时间戳派生）。
func TestWBAINewMessageIDIsRandom(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := newMessageIDWBAI()
		if len(id) != 32 {
			t.Fatalf("id %q 长度 %d != 32", id, len(id))
		}
		for _, c := range id {
			if !strings.ContainsRune("0123456789abcdef", c) {
				t.Fatalf("id %q 含非 hex 字符 %c", id, c)
			}
		}
		if seen[id] {
			t.Fatalf("id 重复：%q（熵不足）", id)
		}
		seen[id] = true
	}
}
