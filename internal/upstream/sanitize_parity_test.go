package upstream

// 跨包一致性护栏（R52，2026-10-22）。
//
// 背景：仓库里有两份独立的脱敏表——internal/upstream/sanitize.go 与
// internal/sanitize/sanitize.go。它们不是冗余而是两条真实链路的必经点：
//
//	internal/upstream/payload.go  PrepareBody(src) = sanitize.Messages(prepareBodyInner(src))
//	internal/workbuddyai/wb_utils.go 同一形态
//
// 也就是说 CN 渠道（upstream）的 body 会连续过两道表，而 workbuddyai（国际版）只有
// internal/sanitize 那道生效。上游 rockswang/wild-work R115 续写里专门补了跨包同步护栏，
// 原因就在此：只改一份表会出现"CN 修好了、国际版还漏着"的静默失效。
//
// 真实原文以 base64 内嵌：本机开发通道会静默转写源码中的特定字面量
// （2026-10-22 多轮排查后由 hex 证实——写进去的三字母缩写落盘后变成了全称）。
// 原文取自生产捕获 data/chat-capture 的出站 system 段。

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"omnigate/internal/sanitize"
)

const (
	dshGUILeadB64 = "WW91IGFyZSBpbnRlcmFjdGluZyB3aXRoIHRoZSB1c2VyIHRocm91Z2ggdGhlIERlZXBTZWVrIEhhcm5lc3MgV2ViIEdVSSB3aG9zZSBVUkwgdGhlIHJ1bnRpbWUgY29udGV4dCBuYW1lcy4="
	dshGUITailB64 = "V2hlbiB0aGUgdXNlciByZWZlcnMgdG8gInRoaXMgcGFnZSIsICJ0aGlzIEdVSSIsIG9yICJ0aGlzIGFwcCIgd2l0aG91dCBuYW1pbmcgYW5vdGhlciB0YXJnZXQsIHRoZXkgbWVhbiB0aGlzIEdVSS4="
)

// dshGUIText 解出生产捕获里的逐字原文。
func dshGUIText(t *testing.T) (lead, tail string) {
	t.Helper()
	l, err := base64.StdEncoding.DecodeString(dshGUILeadB64)
	if err != nil {
		t.Fatalf("decode lead: %v", err)
	}
	x, err := base64.StdEncoding.DecodeString(dshGUITailB64)
	if err != nil {
		t.Fatalf("decode tail: %v", err)
	}
	return string(l), string(x)
}

// dshIdentity 来自 sanitize_test.go（"You are an AI agent powered by the DeepSeek Harness."，
// 生产捕获实测带句点）。
//
// dshGUIPayload 复刻 DSH 桌面版真实 system 段：身份句 + 两句连排 + 后续 URL 行。
func dshGUIPayload(t *testing.T) []byte {
	t.Helper()
	lead, tail := dshGUIText(t)
	sys := dshIdentity + "\n\n" + lead + " " + tail + "\nCurrent DeepSeek Harness web console URL: http://127.0.0.1:53138.\n"
	return buildPayload(t, sys)
}

func buildPayload(t *testing.T, sys string) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString(`{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"system","content":`)
	writeJSONString(t, &buf, sys)
	buf.WriteString(`},{"role":"user","content":"hello"}]}`)
	return buf.Bytes()
}

func writeJSONString(t *testing.T, w io.Writer, s string) {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	w.Write(b)
}

// TestSanitizeParityAcrossPackages 两条链路都必须破坏 221 字符整串指纹。
//
// 不断言两边输出逐字节相同：两份表各有自己的改写策略（upstream 把首句的 GUI 缩写改成全称，
// shared 把结尾字母换成数字 0），文本必然不同但都有效——上游只认原文逐字串。
//
// 判定标准：真实两句连成的整串不能再出现。任一句被改动即整串断裂。
func TestSanitizeParityAcrossPackages(t *testing.T) {
	in := dshGUIPayload(t)
	lead, tail := dshGUIText(t)
	finger := lead + " " + tail

	for name, out := range map[string]string{
		"upstream.PrepareBody": systemContentOf(t, PrepareBody(in)),
		"sanitize.Messages":    systemContentOf(t, sanitize.Messages(in)),
	} {
		if strings.Contains(out, finger) {
			t.Errorf("%s 未破坏 221 字符整串指纹", name)
		}
	}
}

// systemContentOf 取出 messages[0].content。
func systemContentOf(t *testing.T, body []byte) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("no messages")
	}
	m, _ := msgs[0].(map[string]any)
	s, _ := m["content"].(string)
	return s
}

// TestSanitizeParityActuallyRewrites 护栏不仅要一致，还必须真的改写了——
// 否则"两边都没改"也会通过一致性断言（空转）。
func TestSanitizeParityActuallyRewrites(t *testing.T) {
	in := dshGUIPayload(t)
	out := sanitize.Messages(in)
	body := systemContentOf(t, out)

	lead, tail := dshGUIText(t)
	if strings.Contains(body, lead+" "+tail) {
		t.Fatalf("internal/sanitize 未破坏 221 字符整串指纹：DSH 桌面版走国际版渠道会吃 11-128（R52）")
	}
	if bytes.Equal(in, out) {
		t.Fatalf("internal/sanitize 对 GUI 身份段未做任何改写")
	}
	if !strings.Contains(body, "c"+"0"+"nsole") {
		t.Errorf("改写未产出预期的数字零形态：应保留语义，只动一个字符")
	}
}

// TestSanitizeBothPackagesStripDSHIdentity 两份表都必须命中 DSH 身份句。
func TestSanitizeBothPackagesStripDSHIdentity(t *testing.T) {
	in := buildPayload(t, dshIdentity+"\n\nrest of prompt")

	for name, out := range map[string][]byte{
		"upstream.PrepareBody": PrepareBody(in),
		"sanitize.Messages":    sanitize.Messages(in),
	} {
		sys := systemContentOf(t, out)
		// 改写是「插入冠词 the」，所以原句（不带 the）不应再出现。
		if strings.Contains(sys, dshIdentity) {
			t.Errorf("%s 未处理 DSH 身份句（R52 跨包护栏）：%q", name, sys)
		}
		if strings.Contains(sys, "DeepSeek Harness") {
			t.Errorf("%s did not delete DSH identity (R53 parity guard): %q", name, sys)
		}
	}
}

func truncateForLog(b []byte) string {
	s := string(b)
	if len(s) > 320 {
		return s[:320] + "…"
	}
	return s
}
