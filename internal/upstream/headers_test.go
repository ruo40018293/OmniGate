// headers_test.go R49 回归：CN 会话头三件套完整性 + R54 异常取证行为。
package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"omnigate/internal/auth"
	"omnigate/internal/capture"
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
			t.Errorf("%s 缺失或为空：上游渠道校验会回 400 code=11-128（R44/R49）", k)
		}
	}
	if req.Header.Get("X-Refresh-Token") != "" {
		t.Errorf("对话请求禁止携带 X-Refresh-Token")
	}
	if got := req.Header.Get("Authorization"); got != "Bearer at" {
		t.Errorf("Authorization=%q", got)
	}
}

// TestChatStreamCapturesOnlyOnError 是 R54 的核心语义回归：
//
//	正常请求 -> 零写盘（不允许像 R49 那样全量落盘，会堆满磁盘）
//	上游 4xx/5xx -> 必落盘，且同时含出站字节与上游回包原文
//
// 把 capture.Save 的调用删掉，第二个断言必失败；把 enabled() 改成恒 true
// 且不判异常（即退回 R49 全量捕获），第一个断言必失败。
func TestChatStreamCapturesOnlyOnError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(capture.EnvDir, dir)
	t.Setenv(capture.EnvOff, "")
	capture.ResetDirCache()

	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)

	// --- 正常 200：零写盘 ---
	okClient := testClient(func(r *http.Request) (*http.Response, error) {
		io.Copy(io.Discard, r.Body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	rc, status, _, err := okClient.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, body)
	if err != nil || status != 200 {
		t.Fatalf("正常请求失败: status=%d err=%v", status, err)
	}
	rc.Close()
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("正常请求不得写盘，却有 %d 个文件（退化成 R49 全量捕获）", len(ents))
	}

	// --- 上游 400 11-128：必须落盘 ---
	const upBody = `{"code":"11-128","msg":"Illegal API invocation from an unapproved channel"}`
	badClient := testClient(func(r *http.Request) (*http.Response, error) {
		io.Copy(io.Discard, r.Body)
		return &http.Response{
			StatusCode: 400,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(upBody)),
		}, nil
	})
	_, status, _, err = badClient.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, body)
	if err != nil || status != 400 {
		t.Fatalf("异常请求状态不对: status=%d err=%v", status, err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "err-*.json"))
	if len(files) != 1 {
		t.Fatalf("上游 400 应恰有 1 个异常捕获文件，实际 %d（取证未接入）", len(files))
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("捕获文件不是合法 JSON: %v", err)
	}
	if rec["channel"] != "workbuddy" {
		t.Errorf("channel=%v，期望 workbuddy", rec["channel"])
	}
	if rec["uid"] != "u1" {
		t.Errorf("uid=%v，期望 u1（缺了无法定位账号）", rec["uid"])
	}
	req, _ := rec["request"].(string)
	if !strings.Contains(req, `"stream":true`) {
		t.Errorf("捕获的请求体不是出站字节（缺 PrepareBody 的 stream 强制改写）: %s", req)
	}
	resp, _ := rec["response"].(string)
	if !strings.Contains(resp, "11-128") {
		t.Errorf("捕获内容缺上游回包原文: %s", resp)
	}
}
