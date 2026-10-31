package raccoon

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"omnigate/internal/auth"
	"strings"
	"testing"
)

// R86：这三个常量一律 base64 内嵌——本机开发通道会静默转写源码里的指纹字面量
// （见 AGENTS.md 不变量 17），写字面量等于测试自带假阴性。
const r86LeadB64 = "WW91IGFyZSBpbnRlcmFjdGluZyB3aXRoIHRoZSB1c2VyIHRocm91Z2ggdGhlIERlZXBTZWVrIEhhcm5lc3MgV2ViIEdVSSB3aG9zZSBVUkwgdGhlIHJ1bnRpbWUgY29udGV4dCBuYW1lcy4="
const r86TailB64 = "V2hlbiB0aGUgdXNlciByZWZlcnMgdG8gInRoaXMgcGFnZSIsICJ0aGlzIEdVSSIsIG9yICJ0aGlzIGFwcCIgd2l0aG91dCBuYW1pbmcgYW5vdGhlciB0YXJnZXQsIHRoZXkgbWVhbiB0aGlzIEdVSS4="
const r86Mark1B64 = "RGVlcFNlZWsgSGFybmVzcyBXZWIgR1VJ"
const r86Mark2B64 = "dGhleSBtZWFuIHRoaXMgR1VJ"

func r86Dec(t *testing.T, s string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return string(b)
}

// r86ContextBody 造一个带 DSH 运行时上下文的 OpenAI 形状请求体。
func r86ContextBody(t *testing.T) []byte {
	t.Helper()
	content := r86Dec(t, r86LeadB64) + " " + r86Dec(t, r86TailB64)
	m := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": content},
		},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// r86AssertSanitized 断言指纹已被吃掉，且结果仍是合法 JSON。
func r86AssertSanitized(t *testing.T, out []byte) {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	s := string(out)
	for name, b64 := range map[string]string{
		"DeepSeek Harness Web GUI": r86Mark1B64,
		"they mean this GUI":       r86Mark2B64,
	} {
		if strings.Contains(s, r86Dec(t, b64)) {
			t.Fatalf("R86: fingerprint %q still present in outbound body", name)
		}
	}
}
func TestR86RaccoonChatStreamSanitizesContext(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	c := New()
	c.Base = srv.URL
	c.StreamHTTP = srv.Client()
	c.HTTP = srv.Client()
	if _, _, _, err := c.ChatStream(&auth.Auth{}, r86ContextBody(t)); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got == "" {
		t.Fatalf("server saw no body")
	}
	r86AssertSanitized(t, []byte(got))
}

func TestR86RaccoonKeepsNonJSON(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	c := New()
	c.Base = srv.URL
	c.StreamHTTP = srv.Client()
	c.HTTP = srv.Client()
	const raw = "{not json"
	if _, _, _, err := c.ChatStream(&auth.Auth{}, []byte(raw)); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got != raw {
		t.Fatalf("non-JSON body must pass through unchanged, got %q", got)
	}
}
