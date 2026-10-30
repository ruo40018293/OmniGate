package sanitize

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestBodyFallbackRewritesHiddenFields 验证整包兜底能碰到 messages 树遍历不到的字段：
// tools[] 描述、metadata、以及顶层其它角落里的 DSH 身份词。
func TestBodyFallbackRewritesHiddenFields(t *testing.T) {
	name := string([]byte{0x74, 0x6f, 0x6f, 0x6c, 0x41}) // "toolA"
	// 用字节构造 "deepseek-harness" 的几种连字符变体（含 U+2011），避免字面量被改写
	variants := [][]byte{
		append([]byte("deepseek"), append([]byte{0x2d}, []byte("harness")...)...),             // ASCII -
		append([]byte("deepseek"), append([]byte{0xe2, 0x80, 0x91}, []byte("harness")...)...), // U+2011
		append([]byte("deepseek"), append([]byte{0x20}, []byte("harness")...)...),             // space
		[]byte("DEEPSEEK-HARNESS"), // upper
	}
	for i, v := range variants {
		body := []byte(`{"tools":[{"name":"` + name + `","description":"made by ` + string(v) +
			`"}],"messages":[{"role":"user","content":"hi"}]}`)
		got := BodyFallback(body)
		if strings.Contains(strings.ToLower(string(got)), strings.ToLower(string(v))) {
			t.Fatalf("variant %d still present after BodyFallback: %s", i, got)
		}
		var probe map[string]any
		if err := json.Unmarshal(got, &probe); err != nil {
			t.Fatalf("variant %d produced invalid JSON: %v (%s)", i, err, got)
		}
	}
}

// TestBodyFallbackZeroAllocOnClean 干净 body 必须原样返回（不变式 16）。
func TestBodyFallbackZeroAllocOnClean(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`)
	got := BodyFallback(body)
	if string(got) != string(body) {
		t.Fatalf("clean body was modified:\n got=%s\nwant=%s", got, body)
	}
	// 同一底层数组的零分配性质：返回的切片应与入参共享存储
	if &got[0] != &body[0] {
		t.Fatal("clean body should be returned as-is (same backing array)")
	}
}
