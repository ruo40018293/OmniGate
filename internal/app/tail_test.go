package app

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func writeLogLines(t *testing.T, n int) string {
	t.Helper()
	fp := filepath.Join(t.TempDir(), "app.log")
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "2026/10/29 line %04d\n", i)
	}
	if err := os.WriteFile(fp, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("写测试日志失败: %v", err)
	}
	return fp
}

// TestTailLinesTakesLast 尾部读取的正面用例：取最后 n 行、次序不变、无空行。
func TestTailLinesTakesLast(t *testing.T) {
	fp := writeLogLines(t, 1000)
	got := tailLines(fp, 300)
	if len(got) != 300 {
		t.Fatalf("应回 300 行，实际 %d", len(got))
	}
	if got[0] != "2026/10/29 line 0701" {
		t.Fatalf("首行应为第 701 行，实际 %q", got[0])
	}
	if got[len(got)-1] != "2026/10/29 line 1000" {
		t.Fatalf("末行应为第 1000 行，实际 %q", got[len(got)-1])
	}
	for i, l := range got {
		if l == "" {
			t.Fatalf("第 %d 行是空行（末尾换行没被吃掉）", i)
		}
	}
}

// TestTailLinesNoHalfLine 窗口边界：文件远大于 256KB 尾窗口时，首行必须是完整
// 行——从中间开始读出的半行要被丢掉，否则前端会贴出一行被腰斩的日志。
func TestTailLinesNoHalfLine(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "app.log")
	var b strings.Builder
	// 3000 行 × 约 100 字节 ≈ 300KB，超过 256KB 窗口。
	for i := 1; i <= 3000; i++ {
		fmt.Fprintf(&b, "2026/10/29 %04d %s\n", i, strings.Repeat("x", 80))
	}
	if err := os.WriteFile(fp, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("写测试日志失败: %v", err)
	}
	if fi, err := os.Stat(fp); err != nil || fi.Size() <= 256<<10 {
		t.Fatalf("前置条件不成立：文件应大于 256KB，实际 size=%d err=%v", fi.Size(), err)
	}
	got := tailLines(fp, 5)
	if len(got) != 5 {
		t.Fatalf("应回 5 行，实际 %d", len(got))
	}
	for i, l := range got {
		want := fmt.Sprintf("2026/10/29 %04d ", 2996+i)
		if !strings.HasPrefix(l, want) {
			t.Fatalf("第 %d 行应以 %q 开头（不能是半行），实际 %q", i, want, l)
		}
	}
	// 再要一次「窗口内的全部行」：被腰斩的半行若没丢，会作为第一条露出来。
	// 只要 5 行时它会被裁掉看不见，这个断言才是真正钉住丢半行逻辑的那根钉子。
	all := tailLines(fp, 5000)
	if len(all) < 100 {
		t.Fatalf("256KB 窗口内应有多行，实际只有 %d", len(all))
	}
	full := regexp.MustCompile(`^2026/10/29 \d{4} x{80}$`)
	for i, l := range all {
		if !full.MatchString(l) {
			t.Fatalf("第 %d 行不是完整行（半行没被丢掉）：%q", i, l)
		}
	}
}

// TestTailLinesSmallFile 文件比窗口小：整文件读出，行数不足 n 时全给。
func TestTailLinesSmallFile(t *testing.T) {
	fp := writeLogLines(t, 3)
	got := tailLines(fp, 300)
	if len(got) != 3 {
		t.Fatalf("应回 3 行，实际 %d（%v）", len(got), got)
	}
	if got[0] != "2026/10/29 line 0001" || got[2] != "2026/10/29 line 0003" {
		t.Fatalf("内容错误：%v", got)
	}
}

// TestTailLinesCRLF Windows 行尾不把 \r 带进前端。
func TestTailLinesCRLF(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(fp, []byte("a\r\nb\r\nc\r\n"), 0o644); err != nil {
		t.Fatalf("写测试日志失败: %v", err)
	}
	got := tailLines(fp, 10)
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("应回 %d 行，实际 %d（%v）", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 行应为 %q，实际 %q", i, want[i], got[i])
		}
	}
}

// TestTailLinesDegenerate 缺文件 / 空文件 / 非法 n 一律回 nil，不 panic。
func TestTailLinesDegenerate(t *testing.T) {
	if got := tailLines(filepath.Join(t.TempDir(), "nope.log"), 10); got != nil {
		t.Fatalf("缺文件应回 nil，实际 %v", got)
	}
	empty := filepath.Join(t.TempDir(), "empty.log")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatalf("写空文件失败: %v", err)
	}
	if got := tailLines(empty, 10); got != nil {
		t.Fatalf("空文件应回 nil，实际 %v", got)
	}
	fp := writeLogLines(t, 5)
	if got := tailLines(fp, 0); got != nil {
		t.Fatalf("n=0 应回 nil，实际 %v", got)
	}
	if got := tailLines(fp, -1); got != nil {
		t.Fatalf("n<0 应回 nil，实际 %v", got)
	}
}
