package workbuddyai

import (
	"net/http"
	"testing"
)

// TestChatHeadersConversationFamily 回归 R44：上游「渠道校验」要求三个会话头
// 同时存在，缺任意一个返回 400 code=11-128「request illegal / 不是官方客户端
// 发出」。历史上只发了 Request-ID 与 Message-ID，漏了 X-Conversation-ID，
// 导致全部对话请求被拦（2026-10-20 生产现象，本机对照实验定位）。
func TestChatHeadersConversationFamily(t *testing.T) {
	var got http.Header
	srv := testServer(func(w http.ResponseWriter, r *http.Request, n int) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	defer srv.Close()

	c := NewWithBase(srv.URL)
	rc, status, _, err := c.ChatStream(testAuth(), []byte(`{"model":"hy3","messages":[]}`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	if rc != nil {
		rc.Close()
	}

	// 三个会话头缺一不可，空值等同于缺失（上游按「非空」判定是否为官方客户端）。
	for _, name := range []string{
		"X-Conversation-Id",
		"X-Conversation-Request-Id",
		"X-Conversation-Message-Id",
	} {
		if v := got.Get(name); v == "" {
			t.Errorf("%s 缺失或为空：上游会回 400 code=11-128", name)
		}
	}
}

// TestChatHeadersNoRefreshToken 安全红线：对话请求绝不携带 X-Refresh-Token。
func TestChatHeadersNoRefreshToken(t *testing.T) {
	var got http.Header
	srv := testServer(func(w http.ResponseWriter, r *http.Request, n int) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	defer srv.Close()

	c := NewWithBase(srv.URL)
	rc, status, _, err := c.ChatStream(testAuth(), []byte(`{"model":"hy3","messages":[]}`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	if rc != nil {
		rc.Close()
	}
	if v := got.Get("X-Refresh-Token"); v != "" {
		t.Fatalf("对话请求携带了 X-Refresh-Token：%q", v)
	}
	if v := got.Get("Authorization"); v != "Bearer test-token" {
		t.Fatalf("Authorization = %q, want Bearer test-token", v)
	}
}
