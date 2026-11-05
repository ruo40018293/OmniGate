package sanitize

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

func TestProbeTableBytes(t *testing.T) {
	fmt.Printf("PROBE count features=%d rewrites=%d"+probeNL, len(featHeaders), len(rewrites))

	for i := range featHeaders {
		fmt.Printf("PROBE feature[%d] len=%d % x"+probeNL, i, len(featHeaders[i]), []byte(featHeaders[i]))
	}

	effective := 0
	for i := range rewrites {
		o, n := []byte(rewrites[i][0]), []byte(rewrites[i][1])
		same := string(o) == string(n)
		if !same {
			effective++
		}
		fmt.Printf("PROBE rewrite[%d] oldLen=%d newLen=%d identical=%v"+probeNL, i, len(o), len(n), same)
		fmt.Printf("  old % x"+probeNL, o)
		fmt.Printf("  new % x"+probeNL, n)
	}

	fmt.Printf("PROBE regex reHdr len=%d % x"+probeNL, len(reHdr.String()), []byte(reHdr.String()))
	fmt.Printf("PROBE regex reBareHdr len=%d % x"+probeNL, len(reBareHdr.String()), []byte(reBareHdr.String()))
	fmt.Printf("PROBE regex reKv len=%d % x"+probeNL, len(reKv.String()), []byte(reKv.String()))
	fmt.Printf("PROBE regex flexDSHRe len=%d % x"+probeNL, len(flexDSHRe.String()), []byte(flexDSHRe.String()))

	fmt.Printf("PROBE regex identityRe len=%d % x"+probeNL, len(identityRe.String()), []byte(identityRe.String()))
	fmt.Printf("PROBE effective rewrites = %d/%d"+probeNL, effective, len(rewrites))

	// Guards below exist because a broken table still compiles and the rest of
	// the package still passes -- without them this class of damage is silent.
	if len(featHeaders) == 0 {
		t.Fatal("feature table empty: precheck can never match, sanitizer is a no-op")
	}
	if len(rewrites) == 0 {
		t.Fatal("rewrite table empty: nothing is scrubbed")
	}
	for i := range featHeaders {
		if len(featHeaders[i]) == 0 {
			t.Fatalf("feature[%d] is the empty string: a Contains against it is always true, so every request would be flagged", i)
		}
	}
	for i := range rewrites {
		if len(rewrites[i][0]) == 0 {
			t.Fatalf("rewrite[%d] old is empty: ReplaceAll against the empty string injects the target between every byte", i)
		}
	}
	if effective == 0 {
		t.Fatalf("all %d rewrites are old==new, table looks neutralized", len(rewrites))
	}
}
