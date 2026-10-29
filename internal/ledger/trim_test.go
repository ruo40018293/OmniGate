package ledger

// R69：当月流水裁剪（trimCurrentMonth）的回归测试。
//
// 原实现把快速路径判断写反了（首行 ts <= cutTs 就 continue），而文件是
// append-only、首行正是最旧的一条 —— 只要存在过期条目，首行必然 <= cutTs，
// 于是每次都判定「无需裁剪」，7 天裁剪从未生效（实测 usage-202610.jsonl
// 留着 16 天数据）。这里锁住：过期条目被删、边界内条目保留。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTrimCurrentMonthDropsExpiredEntries(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	fp := filepath.Join(dir, fmt.Sprintf("usage-%s.jsonl", now.Format("200601")))

	old := now.AddDate(0, 0, -10).Unix() // 早于 keepDays(7) 边界
	fresh := now.AddDate(0, 0, -1).Unix()
	body := fmt.Sprintf(
		"{\"ts\":%d,\"ch\":\"raccoon\",\"uid\":\"u\",\"model\":\"old\",\"pt\":1,\"ct\":1}\n"+
			"{\"ts\":%d,\"ch\":\"raccoon\",\"uid\":\"u\",\"model\":\"fresh\",\"pt\":2,\"ct\":2}\n",
		old, fresh)
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatalf("写流水: %v", err)
	}

	l, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(l.Close)

	got, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("读流水: %v", err)
	}
	if strings.Contains(string(got), `"model":"old"`) {
		t.Fatalf("10 天前的条目未被裁剪: %s", got)
	}
	if !strings.Contains(string(got), `"model":"fresh"`) {
		t.Fatalf("边界内条目被误删: %s", got)
	}
}

func TestTrimCurrentMonthNoopWhenAllFresh(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	fp := filepath.Join(dir, fmt.Sprintf("usage-%s.jsonl", now.Format("200601")))
	body := fmt.Sprintf("{\"ts\":%d,\"ch\":\"raccoon\",\"uid\":\"u\",\"model\":\"a\",\"pt\":1,\"ct\":1}\n", now.Unix())
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatalf("写流水: %v", err)
	}

	l, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(l.Close)

	got, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("读流水: %v", err)
	}
	if string(got) != body {
		t.Fatalf("全部在边界内时不该改写文件:\n got=%q\nwant=%q", got, body)
	}
}
