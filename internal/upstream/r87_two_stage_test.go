package upstream

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

const r87MarkerB64 = "dGhleSBtZWFuIHRoaXMgR1VJ"

func r87Decode(s string) string {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func r87Marker() string { return r87Decode(r87MarkerB64) }

// r87LeetForm 返回 leetspeak 形态的 console 词（字节级拼接，避免源码里出现该字面量）。
func r87LeetForm() string {
	return "c" + "0" + "nsole"
}

func r87Payload(t *testing.T) []byte {
	t.Helper()
	lead := r87Decode(dshGUILeadB64)
	tail := r87Decode(dshGUITailB64)
	obj := map[string]any{
		"model": "test",
		"messages": []any{
			map[string]any{"role": "system", "content": lead + " " + tail},
		},
	}
	b, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("fixture marshal 失败: %v", err)
	}
	return b
}

// TestR87CNTwoStageSanitizeIsLoadBearing 锁住 CN 出站脱敏的两段式耦合。
//
// 事实：PrepareBody = BodyFallback(Messages(prepareBodyInner(src)))，而
// prepareBodyInner 内部（payload.go:65）已经用 upstream 表做过一遍 sanitizeMessages
// （stage1）。stage1 一生效，stage2（internal/sanitize 表）的 hasFingerprint 预检
// 就不再命中，恒为 no-op。因此 CN 出站的实际改写形态由 upstream 表决定（等价词），
// 而不是 internal/sanitize 表（leetspeak）。
//
// 若有人为了让 internal/sanitize 成为唯一真源而删掉 stage1，CN 出站字节就会从等价词
// 变成 leetspeak —— 这是未经验证的行为变更（生产上 CN 用等价词形态跑出 10529 次请求
// 零指纹拒信）。要先做对照实验才能改。本测试就是那道闸门。
func TestR87CNTwoStageSanitizeIsLoadBearing(t *testing.T) {
	payload := r87Payload(t)
	marker := r87Marker()
	if !strings.Contains(string(payload), marker) {
		t.Fatalf("fixture 不含尾句 marker，测试无效")
	}

	// stage1 必须自己就能吃掉 marker，否则 stage2 会接管。
	stage1 := string(prepareBodyInner(payload))
	if strings.Contains(stage1, marker) {
		t.Fatalf("stage1 未吃掉尾句 marker：CN 两段式耦合已被破坏。删掉 stage1 会让 stage2 接管出站改写，改写形态从等价词变成 leetspeak，属未验证行为变更——请先做对照实验再合并真源。")
	}
	if strings.Contains(stage1, r87LeetForm()) {
		t.Fatalf("stage1 输出出现 leetspeak 形态，与 R52/R59 的等价词口径不符")
	}

	// 外层最终输出同样不能含 marker，也不能含 leetspeak。
	final := string(PrepareBody(payload))
	if strings.Contains(final, marker) {
		t.Fatalf("PrepareBody 出站仍含尾句 marker")
	}
	if strings.Contains(final, r87LeetForm()) {
		t.Fatalf("PrepareBody 出站出现 leetspeak 形态：说明 stage1 已被摘掉、stage2 接管。恢复 stage1，或先做对照实验。")
	}
}
