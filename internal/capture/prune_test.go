package capture

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeCapture 造一个指定大小的 .json 文件，并给它一个明确递增的修改时间。
func writeCapture(t *testing.T, dir, name string, size int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o644); err != nil {
		t.Fatalf("造文件 %s: %v", name, err)
	}
}

// ageFiles 按名字顺序把修改时间排成递增（第一个最旧）。
func ageFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	base := time.Now().Add(-time.Hour)
	for i, n := range names {
		ts := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(filepath.Join(dir, n), ts, ts); err != nil {
			t.Fatalf("改 mtime %s: %v", n, err)
		}
	}
}

// setBudgets 临时改目录上限，返回恢复函数。
func setBudgets(files int, bytes int64, slackFiles int, slackBytes int64) func() {
	of, ob, osf, osb := maxFiles, maxBytes, pruneSlackFiles, pruneSlackBytes
	maxFiles, maxBytes, pruneSlackFiles, pruneSlackBytes = files, bytes, slackFiles, slackBytes
	return func() { maxFiles, maxBytes, pruneSlackFiles, pruneSlackBytes = of, ob, osf, osb }
}

func listJSON(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestPruneNoopUnderBudget 两个上限都没超时一个文件都不能删。
func TestPruneNoopUnderBudget(t *testing.T) {
	defer setBudgets(10, 4096, 2, 1024)()
	dir := t.TempDir()
	for _, n := range []string{"a.json", "b.json", "c.json"} {
		writeCapture(t, dir, n, 100)
	}
	pruneIfNeeded(dir)
	if got := len(listJSON(t, dir)); got != 3 {
		t.Fatalf("未超上限不该删：剩 %d 个", got)
	}
}

// TestPruneByFileCount 文件数超限时按修改时间从最旧的删到滞后目标以内。
func TestPruneByFileCount(t *testing.T) {
	defer setBudgets(5, 1<<20, 2, 0)()
	dir := t.TempDir()
	names := []string{"f0.json", "f1.json", "f2.json", "f3.json", "f4.json", "f5.json", "f6.json"}
	for _, n := range names {
		writeCapture(t, dir, n, 10)
	}
	ageFiles(t, dir, names...)
	pruneIfNeeded(dir)

	left := listJSON(t, dir)
	if len(left) != 3 { // wantCount = 5-2
		t.Fatalf("应删到 3 个（5-2），实际剩 %d：%v", len(left), left)
	}
	// 留下的必须是最新的三个：最旧的是 f0，绝不该活下来。
	for _, n := range left {
		if n == "f0.json" || n == "f1.json" {
			t.Fatalf("删错了方向：最旧的 %s 还在，剩 %v", n, left)
		}
	}
}

// TestPruneByBytes 只超字节（文件数远未到上限）时也必须淘汰，
// 这条是 P2 的核心理由：单文件可到 ~900KB，只封文件数等于没封盘。
func TestPruneByBytes(t *testing.T) {
	defer setBudgets(1000, 4096, 0, 1024)()
	dir := t.TempDir()
	names := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		n := "big" + string(rune('a'+i)) + ".json"
		names = append(names, n)
		writeCapture(t, dir, n, 1000) // 10 × 1000B = 10000B，远超 4096
	}
	ageFiles(t, dir, names...)
	pruneIfNeeded(dir)

	left := listJSON(t, dir)
	var total int64
	for _, n := range left {
		info, err := os.Stat(filepath.Join(dir, n))
		if err != nil {
			t.Fatalf("stat %s: %v", n, err)
		}
		total += info.Size()
	}
	if total > 4096-1024 {
		t.Fatalf("修剪后仍超滞后目标：%d 字节，剩 %v", total, left)
	}
	if len(left) != 3 {
		t.Fatalf("10000B 删到 ≤3072B 应剩 3 个，实际 %d：%v", len(left), left)
	}
	if left[0] == "biga.json" {
		t.Fatalf("最旧的 biga.json 不该留下：%v", left)
	}
}

// TestPruneIgnoresOtherFiles 目录里非 .json 的东西既不算数也不该被删。
func TestPruneIgnoresOtherFiles(t *testing.T) {
	defer setBudgets(2, 1000, 0, 0)()
	dir := t.TempDir()
	writeCapture(t, dir, "note.txt", 5000) // 比字节上限还大，但不是我们写的
	writeCapture(t, dir, "a.json", 100)
	writeCapture(t, dir, "b.json", 100)
	pruneIfNeeded(dir)

	if _, err := os.Stat(filepath.Join(dir, "note.txt")); err != nil {
		t.Fatalf("非 .json 文件不该被删：%v", err)
	}
	// 100+100=200 字节仍在 1000 预算内 → 两个都留。
	if got := len(listJSON(t, dir)); got != 2 {
		t.Fatalf("两个小 .json 都在预算内，不该删：剩 %d", got)
	}
}

// TestSaveTriggersBytePrune Save 落盘前会修剪，整个目录不会因为长期常开而爆盘。
func TestSaveTriggersBytePrune(t *testing.T) {
	defer setBudgets(1000, 2048, 0, 0)()
	dir := t.TempDir()
	t.Setenv(EnvDir, dir)
	t.Setenv(EnvOff, "")
	ResetDirCache()

	for i := 0; i < 6; i++ {
		name := "err-2026010" + string(rune('0'+i)) + "-000000.000-test-0.json"
		writeCapture(t, dir, name, 900)
	}
	if p := Save(Record{Channel: "test", Status: 500, Request: make([]byte, 100)}); p == "" {
		t.Fatal("Save 应落盘")
	}
	left := listJSON(t, dir)
	var total int64
	for _, n := range left {
		info, _ := os.Stat(filepath.Join(dir, n))
		if info != nil {
			total += info.Size()
		}
	}
	if total > 2048 {
		t.Fatalf("Save 之后目录仍超字节上限：%d 字节（%d 个文件）", total, len(left))
	}
}
