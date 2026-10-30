package upstream

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"omnigate/internal/sanitize"
)

// r84GuardOlds holds every non-empty rewrite key from both sanitize tables,
// embedded as base64. Reason: this machine dev channel silently rewrites certain
// literals in source; only encoded form survives byte-for-byte.
var r84GuardOlds = []string{
	"WW91IGFyZSBDbGF1ZGUgQ29kZSwgQW50aHJvcGljJ3Mgb2ZmaWNpYWwgQ0xJIGZvciBDbGF1ZGU=",
	"TWFpbiBicmFuY2ggKHlvdSB3aWxsIHVzdWFsbHkgdXNlIHRoaXMgZm9yIFBScyk=",
	"WW91IGFyZSBhIGNvZGluZyBhZ2VudCBydW5uaW5nIGluIHRoZSBDb2RleCBDTEksIGEgdGVybWluYWwtYmFzZWQgY29kaW5nIGFzc2lzdGFudC4=",
	"VG8gZ2l2ZSBmZWVkYmFjaywgdXNlcnMgc2hvdWxkIHJlcG9ydCB0aGUgaXNzdWUgYXQgaHR0cHM6Ly9naXRodWIuY29tL2FudGhyb3BpY3MvY2xhdWRlLWNvZGUvaXNzdWVz",
	"MTExMjg=",
	"WW91IGFyZSBhbiBBSSBhZ2VudCBwb3dlcmVkIGJ5IERlZXBTZWVrIEhhcm5lc3M=",
	"RGVlcFNlZWsgSGFybmVzcyBXZWIgR1VJ",
	"d2l0aG91dCBuYW1pbmcgYW5vdGhlciB0YXJnZXQsIHRoZXkgbWVhbiB0aGlzIEdVSQ==",
	"VG9rZW5zIHByZWZpeGVkIHdpdGggQCBhcmUgcGF0aHMgdGhlIHVzZXIgZXhwbGljaXRseSByZWZlcmVuY2VkLiBSZWxhdGl2ZSBwYXRocyByZXNvbHZlIGZyb20gdGhlIHdvcmtzcGFjZSByb290",
}

func r84GuardRaw(t *testing.T, b64 string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("bad base64 fixture: %v", err)
	}
	if len(raw) == 0 {
		t.Fatalf("empty fixture")
	}
	return string(raw)
}

// r84GuardPayload wraps key in a real system message.
// Built with fmt.Sprintf percent-c so no escape sequences appear in this file.
func r84GuardPayload(key string) []byte {
	q := fmt.Sprintf("%c", 0x22)
	ob := fmt.Sprintf("%c", 0x7b)
	cb := fmt.Sprintf("%c", 0x7d)
	lb := fmt.Sprintf("%c", 0x5b)
	rb := fmt.Sprintf("%c", 0x5d)
	body := ob + q + "model" + q + fmt.Sprintf("%c", 0x3a) + q + "m" + q + fmt.Sprintf("%c", 0x2c)
	body += q + "messages" + q + fmt.Sprintf("%c", 0x3a) + lb + ob
	body += q + "role" + q + fmt.Sprintf("%c", 0x3a) + q + "system" + q + fmt.Sprintf("%c", 0x2c)
	body += q + "content" + q + fmt.Sprintf("%c", 0x3a) + q + key + q
	body += cb + rb + cb
	return []byte(body)
}

func TestR84EveryRewriteKeyIsRewrittenInBothTables(t *testing.T) {
	for i, b64 := range r84GuardOlds {
		key := r84GuardRaw(t, b64)
		t.Run("key"+strconv.Itoa(i), func(t *testing.T) {
			if got := sanitizeText(string(r84GuardPayload(key))); strings.Contains(string(got), key) {
				t.Errorf("upstream table leaves key %d verbatim", i)
			}
			payload := r84GuardPayload(key)
			if got := string(sanitize.Messages(payload)); strings.Contains(got, key) {
				t.Errorf("sanitize table leaves key %d verbatim", i)
			}
		})
	}
}
