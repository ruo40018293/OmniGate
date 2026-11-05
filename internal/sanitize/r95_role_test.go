package sanitize

import (
	"encoding/json"
	"strings"
	"testing"
)

// r95Brands 返回品牌词的几种分隔符变体（含 U+2011），用字节构造避免字面量被工具转写。
func r95Brands() [][]byte {
	seps := [][]byte{
		{0x2d},             // ASCII -
		{0xe2, 0x80, 0x91}, // U+2011 非断行连字符
		{0x20},             // 空格
		{0x5f},             // 下划线
	}
	var out [][]byte
	for _, sep := range seps {
		out = append(out, append([]byte("deepseek"), append(sep, []byte("harness")...)...))
	}
	return append(out, []byte("DEEPSEEK-HARNESS"))
}

// r95Identity 拼一句 DSH 身份句（可选冠词 the）。
func r95Identity(brand []byte, withThe bool) []byte {
	s := []byte("You are an AI agent powered by ")
	if withThe {
		s = append(s, []byte("the ")...)
	}
	return append(s, brand...)
}

// r95IdentityLine 返回一句身份句（空白分隔形态，正是特征表里的那种）。
func r95IdentityLine() string {
	return string(r95Identity(r95Brands()[2], false))
}

func r95RoleBody(t *testing.T, role any, text string) []byte {
	t.Helper()
	msg := map[string]any{"content": text}
	if role != nil {
		msg["role"] = role
	}
	b, err := json.Marshal(map[string]any{
		"model":    "x/y",
		"messages": []any{map[string]any{"role": "system", "content": "sys"}, msg},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestR95SkipsCallerRoles 锁 R95 第一条：user/tool/function 是调用方的真实内容，
// 用户完全可能正常提到这些词（比如让模型解释某个 CLI），改写等于篡改用户数据。
func TestR95SkipsCallerRoles(t *testing.T) {
	id := r95IdentityLine()
	for _, role := range []any{"user", "tool", "function", nil} {
		out := string(Messages(r95RoleBody(t, role, "prefix "+id+" suffix")))
		if !strings.Contains(out, id) {
			t.Fatalf("role=%v: caller content was rewritten by sanitizer: %s", role, out)
		}
	}
}

// TestR95StillCleansPromptRoles 锁第二条：system/developer/assistant 仍必须净化，
// 且大小写与分隔符变体都要覆盖（预检不敏感而替换敏感就是漏洞）。
func TestR95StillCleansPromptRoles(t *testing.T) {
	for _, brand := range r95Brands() {
		for _, withThe := range []bool{false, true} {
			id := string(r95Identity(brand, withThe))
			for _, role := range []any{"system", "developer", "assistant"} {
				out := string(Messages(r95RoleBody(t, role, "prefix "+id+" suffix")))
				if strings.Contains(out, id) {
					t.Fatalf("role=%v/the=%v: identity survived sanitizer: %s", role, withThe, out)
				}
				if !strings.Contains(out, "prefix") || !strings.Contains(out, "suffix") {
					t.Fatalf("role=%v/the=%v: surrounding text was damaged: %s", role, withThe, out)
				}
			}
		}
	}
}

// TestR95NoWholeBodyRewrite 锁第三条：R82 的整包兜底已删除（上游 2026-11-02 的
// 33 次探针确认 tools 字段的品牌串不再触发拦截，而整包替换会永久篡改调用方内容）。
// 现在只要 messages 树里没有可净化角色，body 就一个字节都不许动。
// 如果将来上游又开始拦 tools 字段，正确做法是回到 data/blocked 取证 + 二分重放，
// 而不是恢复「整包替换成另一家厂商名字」这种不可逆改写。
func TestR95NoWholeBodyRewrite(t *testing.T) {
	id := r95IdentityLine()
	body := []byte(`{"tools":[{"type":"function","function":{"name":"f",` +
		`"description":"` + id + `"}}],"messages":[{"role":"user","content":"hello"}]}`)
	if got := Messages(body); string(got) != string(body) {
		t.Fatalf("whole-body rewrite came back:\n got=%s\nwant=%s", got, body)
	}
}

// TestR95KeepsWhitespaceWhenUnchanged 锁第四条：预检命中但替换后内容没变时，
// 不许顺手 TrimSpace（上游 R151：无改写不动正文）。
// 选裸键名当样本，是因为它既命中特征、又被 reBareHdr 原样替换回同一个串 ——
// 这是唯一能走到「命中但没改」这条分支的真实输入。
func TestR95KeepsWhitespaceWhenUnchanged(t *testing.T) {
	const padded = "  x-anthropic-billing-hdr  "
	out := string(Messages(r95RoleBody(t, "system", padded)))
	if !strings.Contains(out, `"  x-anthropic-billing-hdr  "`) {
		t.Fatalf("whitespace was trimmed even though nothing was rewritten: %s", out)
	}
}
