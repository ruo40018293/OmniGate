package app

// R70：日志轮转的边界测试。
//
// 关键不变量：**今天写过的日志绝不能被搬走** —— internal/stats 启动时全量
// 重扫 app.log 重建当天签到事件，搬走就等于今日收入归零。
// 另外两条：体积没超不动、归档只留一份（旧 .1 会被覆盖）。

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeSized(t *testing.T, path string, size int64, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("改 mtime: %v", err)
	}
}

func TestRotateLogStaleBigFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "app.log")
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.Local)
	writeSized(t, fp, logRotateLimit+1, now.AddDate(0, 0, -1)) // 昨天的

	rotated, err := rotateLogIfStale(fp, logRotateLimit, now)
	if err != nil {
		t.Fatalf("轮转: %v", err)
	}
	if !rotated {
		t.Fatalf("昨天的大文件应被轮转")
	}
	if _, err := os.Stat(fp); !os.IsNotExist(err) {
		t.Fatalf("原文件应已改名: %v", err)
	}
	if _, err := os.Stat(fp + logRotateSuffix); err != nil {
		t.Fatalf("归档文件不存在: %v", err)
	}
}

func TestRotateLogKeepsTodayFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "app.log")
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.Local)
	writeSized(t, fp, logRotateLimit+1, now.Add(-time.Minute)) // 今天刚写过

	rotated, err := rotateLogIfStale(fp, logRotateLimit, now)
	if err != nil {
		t.Fatalf("轮转: %v", err)
	}
	if rotated {
		t.Fatalf("今天写过的日志不能轮转（stats 要靠它重扫签到事件）")
	}
	if _, err := os.Stat(fp); err != nil {
		t.Fatalf("原文件不该消失: %v", err)
	}
	if _, err := os.Stat(fp + logRotateSuffix); !os.IsNotExist(err) {
		t.Fatalf("不该产生归档: %v", err)
	}
}

func TestRotateLogSmallFileUntouched(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "app.log")
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.Local)
	writeSized(t, fp, 1024, now.AddDate(0, 0, -3))

	rotated, err := rotateLogIfStale(fp, logRotateLimit, now)
	if err != nil || rotated {
		t.Fatalf("小文件不该轮转: rotated=%v err=%v", rotated, err)
	}
}

func TestRotateLogMissingFileIsNoop(t *testing.T) {
	dir := t.TempDir()
	rotated, err := rotateLogIfStale(filepath.Join(dir, "nope.log"), logRotateLimit, time.Now())
	if err != nil || rotated {
		t.Fatalf("文件不存在应静默: rotated=%v err=%v", rotated, err)
	}
}

func TestRotateLogReplacesOldArchive(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "app.log")
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.Local)
	if err := os.WriteFile(fp+logRotateSuffix, []byte("旧归档"), 0o644); err != nil {
		t.Fatalf("写旧归档: %v", err)
	}
	writeSized(t, fp, logRotateLimit+1, now.AddDate(0, 0, -1))

	if rotated, err := rotateLogIfStale(fp, logRotateLimit, now); err != nil || !rotated {
		t.Fatalf("应轮转: rotated=%v err=%v", rotated, err)
	}
	st, err := os.Stat(fp + logRotateSuffix)
	if err != nil {
		t.Fatalf("归档不存在: %v", err)
	}
	if st.Size() != logRotateLimit+1 {
		t.Fatalf("旧归档应被覆盖: got %d want %d", st.Size(), logRotateLimit+1)
	}
}
