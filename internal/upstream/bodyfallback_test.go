package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestPrepareBodyStripsDSHFromTools 端到端锁住 R82：tools[] 里的身份词
// 不属于 messages 树，只有 BodyFallback 整包兜底能碰到。
// PrepareBody 必须真的接上了兜底，否则指纹裸奔出网 → 400 code=11-128。
func TestPrepareBodyStripsDSHFromTools(t *testing.T) {
	variants := [][]byte{
		append([]byte("deepseek"), append([]byte{0x2d}, []byte("harness")...)...),             // ASCII -
		append([]byte("deepseek"), append([]byte{0xe2, 0x80, 0x91}, []byte("harness")...)...), // U+2011
		[]byte("DEEPSEEK-HARNESS"), // upper
	}
	for i, v := range variants {
		src := []byte(`{"model":"m","tools":[{"type":"function","function":{"name":"f",` +
			`"description":"powered by ` + string(v) + `"}}],` +
			`"messages":[{"role":"user","content":"hi"}]}`)

		got := PrepareBody(src)

		if strings.Contains(strings.ToLower(string(got)), strings.ToLower(string(v))) {
			t.Fatalf("variant %d: DSH identity survived PrepareBody:\n%s", i, got)
		}
		var probe map[string]any
		if err := json.Unmarshal(got, &probe); err != nil {
			t.Fatalf("variant %d: PrepareBody produced invalid JSON: %v\n%s", i, err, got)
		}
		// 兜底不能把用户的正常内容吃掉
		if !strings.Contains(string(got), `"name":"f"`) {
			t.Fatalf("variant %d: tool name was damaged:\n%s", i, got)
		}
	}
}
