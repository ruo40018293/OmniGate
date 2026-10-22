// headers_test.go R49 回归：CN 会话头三件套完整性 + 出站捕获钩子。
package upstream

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"omnigate/internal/auth"
)

// TestChatHeadersConversationFamily：对话头必须带齐 X-Conversation-ID /
// X-Conversation-Request-ID / X-Conversation-Message-ID（国际版缺任一 400/11-128
// 已实证，见 AGENTS.md R44；国内版灰度对齐 R49）。删掉 injectConversationHeaders
// 里的 X-Conversation-ID 行，本测试必须失败。
func TestChatHeadersConversationFamily(t *testing.T) {
	req, err := http.NewRequest("POST", "https://chat.example/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	ChatHeaders(req, &auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.codebuddy.cn"})
	for _, k := range []string{"X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID"} {
		if req.Header.Get(k) == "" {
			t.Errorf("%s 缺失或为空：上游渠道校验会回 400 code=11128（R44/R49）", k)
		}
	}
	if req.Header.Get("X-Refresh-Token") != "" {
		t.Errorf("对话请求禁止携带 X-Refresh-Token")
	}
	if got := req.Header.Get("Authorization"); got != "Bearer at" {
		t.Errorf("Authorization=%q", got)
	}
}

// TestChatStreamCapturesOutgoingBody：OMNIGATE_CAPTURE_CHAT 指向目录时，
// ChatStream 必须把**净化后的完整出站 body** 落盘（与上游实际收到字节一致），
// 供 11128 复现后逐段二分；env 未设置时必须零写盘。删掉捕获块，第一个断言失败。
func TestChatStreamCapturesOutgoingBody(t *testing.T) {
	dir := t.TempDir()
	c := testClient(func(r *http.Request) (*http.Response, error) {
		io.Copy(io.Discard, r.Body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	// env 关：不产生任何捕获文件
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("未设置 env 却已有文件: %v", entries)
	}
	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)
	rc, status, _, err := c.ChatStream(a, body)
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	rc.Close()
	if entries, _ = os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("env 未设置时不得写盘，却有 %d 个文件", len(entries))
	}

	// env 开：落盘且内容为出站字节（含 PrepareBody 的 stream 强制改写）
	t.Setenv("OMNIGATE_CAPTURE_CHAT", dir)
	rc, status, _, err = c.ChatStream(a, body)
	if err != nil || status != 200 {
		t.Fatalf("chat2: status=%d err=%v", status, err)
	}
	rc.Close()
	files, _ := filepath.Glob(filepath.Join(dir, "chat-*.json"))
	if len(files) != 1 {
		t.Fatalf("应恰有 1 个捕获文件，实际 %d", len(files))
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"stream":true`) {
		t.Errorf("捕获内容不是出站字节（缺 stream 强制改写）: %s", raw)
	}
}
