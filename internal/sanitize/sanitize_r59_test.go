package sanitize

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// r59KeyB64 见 internal/upstream/sanitize_r59_test.go 同名字面量的来源说明。
const r59KeyB64 = "VG9rZW5zIHByZWZpeGVkIHdpdGggQCBhcmUgcGF0aHMgdGhlIHVzZXIgZXhwbGljaXRseSByZWZlcmVuY2VkLiBSZWxhdGl2ZSBwYXRocyByZXNvbHZlIGZyb20gdGhlIHdvcmtzcGFjZSByb290"

// TestAtPathPhraseRewritten 回归 R59（共享表侧）：workbuddyai（国际版）只有这张表生效，
// 必须同步改写，否则国际版上游一旦启用同规则即失守。
func TestAtPathPhraseRewritten(t *testing.T) {
	b, err := base64.StdEncoding.DecodeString(r59KeyB64)
	if err != nil {
		t.Fatalf("base64 解码失败: %v", err)
	}
	finger := string(b)
	sys := "Context: " + finger + "; absolute paths identify files or directories on the host."
	body, err := json.Marshal(map[string]any{
		"model": "deepseek-v4.1-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": sys},
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	if err != nil {
		t.Fatalf("造 body 失败: %v", err)
	}
	out := string(Messages(body))
	if strings.Contains(out, finger) {
		t.Fatalf("出站 body 仍含 111 字符渠道校验指纹（上游会回 400 code=11128）")
	}
	if !strings.Contains(out, "Tokens led by @") {
		t.Fatalf("改写未生效，出站 body 里找不到等价表达")
	}
}
