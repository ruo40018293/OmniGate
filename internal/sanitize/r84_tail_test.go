package sanitize

// R84 regression guard (2026-10-31): internal/sanitize is the table that actually
// runs on the international channel, so it needs its own guard rather than relying
// on internal/upstream covering it.
//
// The bug this pins: the trailing GUI sentence rule keys the match on the
// ALREADY-REWRITTEN form, which never appears in a real request, so the sentence
// ships verbatim. Forensics: the GUI form occurs 45 times across 18 production
// captures, the rewritten form only 12 times across 3.
//
// Fixtures are base64-embedded because this repo development channel silently
// transcribes certain literals in source (AGENTS.md R82).

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

const (
	r84TailB64   = "V2hlbiB0aGUgdXNlciByZWZlcnMgdG8gInRoaXMgcGFnZSIsICJ0aGlzIEdVSSIsIG9yICJ0aGlzIGFwcCIgd2l0aG91dCBuYW1pbmcgYW5vdGhlciB0YXJnZXQsIHRoZXkgbWVhbiB0aGlzIEdVSS4="
	r84MarkerB64 = "dGhleSBtZWFuIHRoaXMgR1VJ"
)

func r84Tail(t *testing.T) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(r84TailB64)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return string(b)
}

// TestR84TailSentenceIsRewrittenHere the rule must survive on this table alone.
func TestR84TailSentenceIsRewrittenHere(t *testing.T) {
	tail := r84Tail(t)
	b, err := base64.StdEncoding.DecodeString(r84MarkerB64)
	if err != nil {
		t.Fatalf("decode marker: %v", err)
	}
	marker := string(b)
	if !strings.Contains(tail, marker) {
		t.Fatalf("fixture drift: tail no longer carries the marker")
	}
	out := string(Messages(r84Payload(t, tail)))
	if strings.Contains(out, marker) {
		t.Errorf("trailing GUI marker ships verbatim (R84): %s", out)
	}
	if !strings.Contains(out, "without naming another target") {
		t.Errorf("sentence was gutted instead of rewritten: %s", out)
	}
}

// r84Payload 最小合法请求体：system 段独占一行 content。
func r84Payload(t *testing.T, sys string) []byte {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"model":"m","stream":true,"messages":[{"role":"system","content":`)
	enc, err := json.Marshal(sys)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	b.Write(enc)
	b.WriteString(`},{"role":"user","content":"hi"}]}`)
	return []byte(b.String())
}
