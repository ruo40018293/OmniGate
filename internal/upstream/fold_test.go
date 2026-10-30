// fold_test.go R82 预检大小写不敏感回归（upstream 平行表）。
//
// 本文件与 internal/sanitize/fold_test.go 成对存在：两份脱敏表是平行实现
// （AGENTS.md R52），任何一侧漏改都会造成「同一请求走不同渠道行为不一致」。
// 输入一律用十六进制构造，理由同另一侧 —— 字面量会被改写。
package upstream

import (
	"encoding/hex"
	"testing"
)

func fromHexUp(t *testing.T, s string) string {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return string(b)
}

// TestAsciiContainsFoldUpstreamBasics helper 语义。
func TestAsciiContainsFoldUpstreamBasics(t *testing.T) {
	mixed := fromHexUp(t, "436c6175646520634f6465") // "Claude cOde"
	lower := fromHexUp(t, "636c6175646520636f6465") // "claude code"
	upper := fromHexUp(t, "434c4155444520434f4445") // "CLAUDE CODE"

	if !asciiContainsFold(mixed, lower) {
		t.Fatal("mixed must match lower needle")
	}
	if !asciiContainsFold(lower, upper) {
		t.Fatal("lower must match upper needle")
	}
	if !asciiContainsFold(upper, lower) {
		t.Fatal("upper must match lower needle")
	}
	if asciiContainsFold(fromHexUp(t, "616263"), fromHexUp(t, "616263646566")) {
		t.Fatal("longer needle must not match")
	}
}

// TestHasFingerprintUpstreamCaseInsensitive 表中每条的大小写变体都必须命中预检。
func TestHasFingerprintUpstreamCaseInsensitive(t *testing.T) {
	if len(sanitizeFeatures) == 0 {
		t.Fatal("sanitizeFeatures must not be empty")
	}
	for i, f := range sanitizeFeatures {
		if len(f) < 4 {
			continue
		}
		var lb, ub []byte
		for j := 0; j < len(f); j++ {
			c := f[j]
			switch {
			case c >= 0x41 && c <= 0x5a:
				lb = append(lb, c+0x20)
				ub = append(ub, c)
			case c >= 0x61 && c <= 0x7a:
				lb = append(lb, c)
				ub = append(ub, c-0x20)
			default:
				lb = append(lb, c)
				ub = append(ub, c)
			}
		}
		if !hasFingerprint("x " + string(lb) + " y") {
			t.Fatalf("sanitizeFeatures[%d] 全小写变体未命中: % x", i, lb)
		}
		if !hasFingerprint("x " + string(ub) + " y") {
			t.Fatalf("sanitizeFeatures[%d] 全大写变体未命中: % x", i, ub)
		}
	}
}

// TestHasFingerprintUpstreamRejectsClean 防止折叠过度造成假阳性。
func TestHasFingerprintUpstreamRejectsClean(t *testing.T) {
	clean := fromHexUp(t, "48656c6c6f2c20706c65617365207772697465206120706f656d2e")
	if hasFingerprint(clean) {
		t.Fatalf("clean text must not trip precheck: % x", clean)
	}
}
