// r101_cleanup_test.go 锁住 R101：cleanup 用 monthTagShift 的 cutoff 删分段文件，
// 保留月数必须真的生效（不能把上个月也删掉）。
package ledger

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCleanupKeepsLastTwoMonths 当月与上月的分段必须保留，更早的才删。
func TestCleanupKeepsLastTwoMonths(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	thisMonth := now.Format("200601")
	lastMonth := monthTagShift(now, 1)
	olderMonth := monthTagShift(now, 3)

	for _, tag := range []string{thisMonth, lastMonth, olderMonth} {
		fp := filepath.Join(dir, "usage-"+tag+".jsonl")
		if err := os.WriteFile(fp, []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("写 %s: %v", fp, err)
		}
	}
	// 非流水分段文件不许动
	other := filepath.Join(dir, "notes.jsonl")
	if err := os.WriteFile(other, []byte("keep me\n"), 0o600); err != nil {
		t.Fatalf("写 notes: %v", err)
	}

	l, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(l.Close)

	for _, tag := range []string{thisMonth, lastMonth} {
		fp := filepath.Join(dir, "usage-"+tag+".jsonl")
		if _, err := os.Stat(fp); err != nil {
			t.Errorf("usage-%s.jsonl 被误删（keepMonths=%d 应保留当月与上月）: %v", tag, keepMonths, err)
		}
	}
	fp := filepath.Join(dir, "usage-"+olderMonth+".jsonl")
	if _, err := os.Stat(fp); err == nil {
		t.Errorf("usage-%s.jsonl 应被裁掉（比 cutoff 早 %d 个月）", olderMonth, keepMonths+2)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("非 usage-*.jsonl 文件被误删: %v", err)
	}
}
