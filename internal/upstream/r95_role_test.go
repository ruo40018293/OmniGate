package upstream

import (
	"strings"
	"testing"
)

// r95Brands 返回品牌词的几种分隔符变体（含 U+2011），用字节构造避免字面量被工具转写。
func r95Brands() [][]byte {
	seps := [][]byte{
		{0x2d},             // ASCII -
		{0xe2, 0x80, 0x91}, // U+2011 非断行连字符
		{0x20},             // 空格
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

func r95RoleBody(role string, text string) []byte {
	roleField := ""
	if role != "" {
		roleField = `"role":"` + role + `",`
	}
	return []byte(`{"model":"m","messages":[{"role":"system","content":"sys"},` +
		`{` + roleField + `"content":"` + text + `"}]}`)
}

// TestR95SkipsCallerRoles 锁 R95 第一条：user/tool/function 是调用方的真实内容，
// 改写这些角色等于篡改用户数据（上游探针确认只拦 system/assistant）。
func TestR95SkipsCallerRoles(t *testing.T) {
	id := string(r95Identity(r95Brands()[2], false))
	for _, role := range []string{"user", "tool", "function", ""} {
		got := string(PrepareBody(r95RoleBody(role, "prefix "+id+" suffix")))
		if !strings.Contains(got, id) {
			t.Fatalf("role=%q: caller content was rewritten: %s", role, got)
		}
	}
}

// TestR95StillCleansPromptRoles 锁第二条：system/developer/assistant 仍必须净化，
// 且大小写与分隔符变体都要覆盖（预检不敏感而替换敏感就是漏洞）。
func TestR95StillCleansPromptRoles(t *testing.T) {
	for _, brand := range r95Brands() {
		for _, withThe := range []bool{false, true} {
			id := string(r95Identity(brand, withThe))
			for _, role := range []string{"system", "developer", "assistant"} {
				got := string(PrepareBody(r95RoleBody(role, "prefix "+id+" suffix")))
				if strings.Contains(got, id) {
					t.Fatalf("role=%q/the=%v: identity survived PrepareBody: %s", role, withThe, got)
				}
				if !strings.Contains(got, "prefix") || !strings.Contains(got, "suffix") {
					t.Fatalf("role=%q/the=%v: surrounding text was damaged: %s", role, withThe, got)
				}
			}
		}
	}
}

// TestR95NoWholeBodyRewrite 锁第三条：整包兜底已删除。messages 树里没有可净化角色时，
// tools 等调用方字段一个字节都不许动。
func TestR95NoWholeBodyRewrite(t *testing.T) {
	id := string(r95Identity(r95Brands()[2], false))
	src := []byte(`{"model":"m","tools":[{"type":"function","function":{"name":"f",` +
		`"description":"` + id + `"}}],"messages":[{"role":"user","content":"hello"}]}`)
	got := string(PrepareBody(src))
	if !strings.Contains(got, id) {
		t.Fatalf("whole-body rewrite came back:\n got=%s", got)
	}
}

// TestR95KeepsWhitespaceWhenUnchanged 锁第四条：预检命中但替换后内容没变时，
// 不许顺手 TrimSpace。裸键名既命中特征、又被 reBareHdr 原样替换回同一个串。
func TestR95KeepsWhitespaceWhenUnchanged(t *testing.T) {
	const padded = "  x-anthropic-billing-hdr  "
	got := string(PrepareBody(r95RoleBody("system", padded)))
	if !strings.Contains(got, `"  x-anthropic-billing-hdr  "`) {
		t.Fatalf("whitespace was trimmed even though nothing was rewritten: %s", got)
	}
}
