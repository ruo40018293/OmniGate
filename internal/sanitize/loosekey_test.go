package sanitize

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestLooseHdrKeyCatchesSeparatorVariants 锁住 R82 第 2 项：
// 指纹发起方在键名各段之间插入非字母分隔符（Unicode 连字符 U+2011、空格、
// 零宽空格 U+200B）来规避写死 ASCII 的检测。宽松正则必须照样识别为指纹，
// 否则整条净化链路被跳过 → 请求裸奔出网 → 网关 400 code=11-128。
func TestLooseHdrKeyCatchesSeparatorVariants(t *testing.T) {
	variants := []struct {
		name string
		key  string
	}{
		{"ascii hyphen", keySegment("-")},
		{"u2011", keySegment("‑")},
		{"spaces", keySegment(" ")},
		{"zwsp", keySegment("​")},
		{"mixed case", "X-Anthropic-BILLING-hdr"},
	}
	for _, v := range variants {
		if !reBareHdr.MatchString(v.key) {
			t.Errorf("%s: bare key %q not matched by reBareHdr", v.name, v.key)
		}
		kv := v.key + ": some-value; trailing"
		if !reHdr.MatchString(kv) {
			t.Errorf("%s: kv form %q not matched by reHdr", v.name, kv)
		}
	}
}

// TestLooseHdrKeyNoFalsePositiveOnProse 反向：宽松不能宽到把正常英文误判成指纹。
// 误判会吞掉用户的正常内容，比漏判更糟——那是在破坏请求语义（违反不变式 16）。
func TestLooseHdrKeyNoFalsePositiveOnProse(t *testing.T) {
	prose := "please extract the anthropic billing header value from the log"
	if reBareHdr.MatchString(prose) {
		t.Errorf("normal prose matched by reBareHdr: %q", prose)
	}
}

// TestMessagesStripsVariantBillingHeader 端到端：变体键名的键值形态必须被**整段吃掉**。
//
// 这里刻意断言「连 anthropic / billing 这两个词都不许留下」，而不是只断言
// 「U+2011 没了」——reBareHdr 单独命中时会把变体键名重写成 ASCII 规范形态留在
// 原地，那种写法看着干净、实则指纹仍在。按设计键值形态必须先被 reHdr 整段删除。
// 正是这个更严的断言在变异中暴露了先前版本的「假绿」。
func TestMessagesStripsVariantBillingHeader(t *testing.T) {
	variant := keySegment("‑") + ": 2026-01-01T00:00:00Z; trailing text"
	raw, err := json.Marshal(map[string]any{
		"model":    "m",
		"messages": []any{map[string]string{"role": "system", "content": variant}},
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	got := Messages(raw)

	var probe struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got, &probe); err != nil {
		t.Fatalf("Messages produced invalid JSON: %v\ngot=%s", err, got)
	}
	if len(probe.Messages) != 1 {
		t.Fatalf("message count=%d want 1\ngot=%s", len(probe.Messages), got)
	}
	content := strings.ToLower(probe.Messages[0].Content)
	if strings.Contains(content, "anthropic") || strings.Contains(content, "billing") {
		t.Fatalf("billing key survived sanitization: %q\nfull=%s", probe.Messages[0].Content, got)
	}
}

// keySegment 用分隔符拼出 billing header 键名。
// 不写字面量：本仓库多个读取通道会截断这个完整键名（AGENTS.md R82 取证）。
func keySegment(sep string) string {
	return strings.Join([]string{"x", "anthropic", "billing", "hdr"}, sep)
}
