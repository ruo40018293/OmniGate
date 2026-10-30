// diag_test.go R82 拦截取证回归：验证 DumpBlocked 的落盘保真、份数守恒、
// 总字节上限（对齐 R75）与空 body 短路。
//
// 全部用例用 t.Chdir 切到临时目录，绝不在仓库里真的造 data/blocked/。
// 样本尺寸一律写字面常量，不由 diagMaxBytes 派生——否则调大上限会让用例
// 申请 TB 级内存直接 OOM，把「变红」变成「拖垮机器」。
package upstream

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// listBlocked 列出当前目录里的全部取证样本。
func listBlocked(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(diagDir, "blocked-*.json"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	return matches
}

// TestDumpBlockedWritesBody 落盘必须是发往上游的原始 body 字节，
// 不能二次编码——bisect-blocked.py 要原样重放。
func TestDumpBlockedWritesBody(t *testing.T) {
	t.Chdir(t.TempDir())

	body := []byte(`{"model":"m","messages":[{"role":"system","content":"hello"}]}`)
	DumpBlocked(body)

	matches := listBlocked(t)
	if len(matches) != 1 {
		t.Fatalf("expected 1 sample, got %d", len(matches))
	}
	got, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("bytes differ:\n got=%q\nwant=%q", got, body)
	}
	var probe map[string]any
	if err := json.Unmarshal(got, &probe); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
}

// TestDumpBlockedIgnoresEmpty 空 body 不落盘、不建目录。
func TestDumpBlockedIgnoresEmpty(t *testing.T) {
	t.Chdir(t.TempDir())

	DumpBlocked(nil)
	DumpBlocked([]byte{})
	if _, err := os.Stat(diagDir); !os.IsNotExist(err) {
		t.Fatalf("empty body must not create %s (stat err=%v)", diagDir, err)
	}
}

// TestDumpBlockedPrunesToKeep 超过 diagKeep 份时只留最新 diagKeep 份。
func TestDumpBlockedPrunesToKeep(t *testing.T) {
	t.Chdir(t.TempDir())

	for i := 0; i < diagKeep+2; i++ {
		DumpBlocked([]byte(`{"n":` + string(rune('0'+i)) + `}`))
		time.Sleep(2 * time.Millisecond)
	}
	if got := len(listBlocked(t)); got != diagKeep {
		t.Fatalf("want %d kept, got %d", diagKeep, got)
	}
}

// TestDumpBlockedKeepsSmallSamples 小样本必须全留，防止字节上限误伤正常取证。
func TestDumpBlockedKeepsSmallSamples(t *testing.T) {
	t.Chdir(t.TempDir())

	small := []byte(`{"messages":[{"role":"user","content":"x"}]}`)
	for i := 0; i < diagKeep; i++ {
		DumpBlocked(small)
		time.Sleep(2 * time.Millisecond)
	}
	if got := len(listBlocked(t)); got != diagKeep {
		t.Fatalf("small samples: want %d kept, got %d", diagKeep, got)
	}
}

// TestDumpBlockedPrunesToByteCap 对齐 R75：只封份数挡不住多模态 base64。
// 两份 1.5MB（合计 3MB，已超 2MB 上限）即便没超 diagKeep 也必须修剪。
// 两份长度故意不同，用来锁住「删旧留新」。
func TestDumpBlockedPrunesToByteCap(t *testing.T) {
	t.Chdir(t.TempDir())

	const sizeOld = 1500 * 1024
	const sizeNew = sizeOld + 1024

	DumpBlocked(bytes.Repeat([]byte("a"), sizeOld))
	time.Sleep(3 * time.Millisecond)
	DumpBlocked(bytes.Repeat([]byte("b"), sizeNew))

	matches := listBlocked(t)
	if len(matches) != 1 {
		t.Fatalf("2 oversized samples (total 3MB > cap): want 1 kept, got %d", len(matches))
	}
	got, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != sizeNew {
		t.Fatalf("survivor must be the NEW sample (len %d), got %d", sizeNew, len(got))
	}
}
