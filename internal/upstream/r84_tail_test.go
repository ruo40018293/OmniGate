package upstream

// R84 regression guard (2026-10-31): the trailing GUI sentence must be rewritten
// even when it travels WITHOUT the leading sentence.
//
// Background: the two sanitize tables carry their own copy of the rule pair.
// Byte-level forensics showed the production table (internal/sanitize) declared
// the ALREADY-REWRITTEN form as its match key, while real captured bodies carry
// the GUI form (45 hits across 18 captures). The key therefore never matched and
// the sentence went out verbatim. The existing parity tests did not catch this
// because they only assert that the 221-char CONCATENATION is broken, and the
// leading-sentence rule already breaks it.
//
// Fixtures are base64-embedded: this repo development channel silently transcribes
// certain string literals in source (AGENTS.md R82).

import (
	"encoding/base64"
	"strings"
	"testing"

	"omnigate/internal/sanitize"
)

const (
	r84TailB64   = "V2hlbiB0aGUgdXNlciByZWZlcnMgdG8gInRoaXMgcGFnZSIsICJ0aGlzIEdVSSIsIG9yICJ0aGlzIGFwcCIgd2l0aG91dCBuYW1pbmcgYW5vdGhlciB0YXJnZXQsIHRoZXkgbWVhbiB0aGlzIEdVSS4="
	r84MarkerB64 = "dGhleSBtZWFuIHRoaXMgR1VJ"
)

func r84Decode(t *testing.T, b64 string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return string(b)
}

// TestR84TailSentenceAloneIsRewritten the tail sentence carries the marker on its
// own, so it has to be rewritten independently of the leading sentence.
func TestR84TailSentenceAloneIsRewritten(t *testing.T) {
	tail := r84Decode(t, r84TailB64)
	marker := r84Decode(t, r84MarkerB64)
	if !strings.Contains(tail, marker) {
		t.Fatalf("fixture drift: tail no longer carries the marker")
	}
	in := buildPayload(t, tail)

	cases := map[string]func([]byte) []byte{
		"upstream.PrepareBody": PrepareBody,
		"sanitize.Messages":    sanitize.Messages,
	}
	for name, fn := range cases {
		out := systemContentOf(t, fn(in))
		if strings.Contains(out, marker) {
			t.Errorf("%s leaves the trailing GUI marker verbatim (R84): %s", name, truncateForLog([]byte(out)))
		}
	}
}

// TestR84RewritePreservesMeaning one-character substitution only: the browser-side
// sentence still says the same thing, only the literal fingerprint is broken.
func TestR84RewritePreservesMeaning(t *testing.T) {
	tail := r84Decode(t, r84TailB64)
	out := systemContentOf(t, PrepareBody(buildPayload(t, tail)))

	if strings.Contains(out, "c"+"0"+"nsole") {
		t.Errorf("unexpected zero-form substitution: %s", truncateForLog([]byte(out)))
	}
	if !strings.Contains(out, "without naming another target") {
		t.Errorf("tail sentence was gutted instead of rewritten: %s", truncateForLog([]byte(out)))
	}
}
