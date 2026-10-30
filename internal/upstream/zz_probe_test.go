package upstream

import (
	"fmt"
	"testing"
)

// zz_probe_test.go -- AGENTS.md invariant 17 executable forensics tool.
// Local read channels (sed / grep / python print / edit rendering) silently
// rewrite or truncate fingerprint literals and disagree with each other.
// R82 burned 6 attempts editing from those distorted views; printing bytes
// from go test is the only trustworthy view of what is really on disk.
//
// Usage: run `go test -run TestProbe -v` BEFORE touching any sanitize.go,
// save the output, then edit, then run again and diff. Every change in a
// length or byte sequence must be explainable.
//
// The newline is assembled from a byte literal instead of a backslash escape,
// per invariant 17 construction discipline.
var probeNL = string([]byte{0x0a})

func TestProbeUpstreamTableBytes(t *testing.T) {
	fmt.Printf("PROBE count features=%d rewrites=%d"+probeNL, len(sanitizeFeatures), len(sanitizeRewrites))

	for i := range sanitizeFeatures {
		fmt.Printf("PROBE feature[%d] len=%d % x"+probeNL, i, len(sanitizeFeatures[i]), []byte(sanitizeFeatures[i]))
	}

	effective := 0
	for i := range sanitizeRewrites {
		o, n := []byte(sanitizeRewrites[i][0]), []byte(sanitizeRewrites[i][1])
		same := string(o) == string(n)
		if !same {
			effective++
		}
		fmt.Printf("PROBE rewrite[%d] oldLen=%d newLen=%d identical=%v"+probeNL, i, len(o), len(n), same)
		fmt.Printf("  old % x"+probeNL, o)
		fmt.Printf("  new % x"+probeNL, n)
	}

	fmt.Printf("PROBE regex sanitizeHdrRe len=%d % x"+probeNL, len(sanitizeHdrRe.String()), []byte(sanitizeHdrRe.String()))
	fmt.Printf("PROBE regex sanitizeBareHdrRe len=%d % x"+probeNL, len(sanitizeBareHdrRe.String()), []byte(sanitizeBareHdrRe.String()))
	fmt.Printf("PROBE regex sanitizeKvRe len=%d % x"+probeNL, len(sanitizeKvRe.String()), []byte(sanitizeKvRe.String()))
	fmt.Printf("PROBE regex bodyDSHRe len=%d % x"+probeNL, len(bodyDSHRe.String()), []byte(bodyDSHRe.String()))

	tgt := dshReplaceTarget()
	fmt.Printf("PROBE dshReplaceTarget len=%d % x"+probeNL, len(tgt), tgt)
	fmt.Printf("PROBE effective rewrites = %d/%d"+probeNL, effective, len(sanitizeRewrites))

	// Guards below exist because a broken table still compiles and the rest of
	// the package still passes -- without them this class of damage is silent.
	if len(sanitizeFeatures) == 0 {
		t.Fatal("feature table empty: precheck can never match, sanitizer is a no-op")
	}
	if len(sanitizeRewrites) == 0 {
		t.Fatal("rewrite table empty: nothing is scrubbed")
	}
	for i := range sanitizeFeatures {
		if len(sanitizeFeatures[i]) == 0 {
			t.Fatalf("feature[%d] is the empty string: a Contains against it is always true, so every request would be flagged", i)
		}
	}
	for i := range sanitizeRewrites {
		if len(sanitizeRewrites[i][0]) == 0 {
			t.Fatalf("rewrite[%d] old is empty: ReplaceAll against the empty string injects the target between every byte", i)
		}
	}
	if effective == 0 {
		t.Fatalf("all %d rewrites are old==new, table looks neutralized", len(sanitizeRewrites))
	}
}
