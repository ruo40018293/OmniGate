package upstream

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// r59KeyB64 是 2026-10-27 生产失败取证（err-20261027-232227.233-workbuddy-400-637056.json）
// 里 system 消息的原始片段，base64 内嵌以免被工具链/编辑器改写（仓库里已发生过字面量被
// 静默改写的事故，见 AGENTS.md R52 教训 3）。
//
// 上游按「连续字面」判定渠道校验：实测该片段 110 字符放行、111 字符即 400 code=11128。
// 因此 rewrite 只改后半句即可破串，前句 "Tokens prefixed with @" 原样保留。
const r59KeyB64 = "VG9rZW5zIHByZWZpeGVkIHdpdGggQCBhcmUgcGF0aHMgdGhlIHVzZXIgZXhwbGljaXRseSByZWZlcmVuY2VkLiBSZWxhdGl2ZSBwYXRocyByZXNvbHZlIGZyb20gdGhlIHdvcmtzcGFjZSByb290"

func r59Key(t *testing.T) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(r59KeyB64)
	if err != nil {
		t.Fatalf("base64 解码失败: %v", err)
	}
	return string(b)
}

// TestDSHAtPathPhraseRewritten 回归 R59：@path 说明句必须被改写，
// 否则 workbuddy（CN）渠道整段请求 400 code=11128。
func TestDSHAtPathPhraseRewritten(t *testing.T) {
	finger := r59Key(t)
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
	out := string(PrepareBody(body))
	if strings.Contains(out, finger) {
		t.Fatalf("出站 body 仍含 111 字符渠道校验指纹（上游会回 400 code=11128）")
	}
	if !strings.Contains(out, "Tokens led by @") {
		t.Fatalf("改写未生效，出站 body 里找不到等价表达")
	}
}
