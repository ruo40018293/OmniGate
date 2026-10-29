package ledger

// R69：DailyCompletion（历史输出量回填的数据源）。
//
// stats.json 的 out_days 是 R66 新增字段，老数据没有输出归档；启动时用这份
// 按天汇总的 completion 回填。这里锁住三点：按本地日切分、只算 CT（不含 PT）、
// 窗口外的条目与 CT=0 的条目都不进结果。

import (
	"testing"
	"time"
)

func TestDailyCompletionSumsCTPerLocalDay(t *testing.T) {
	l := newTestLedger(t)
	now := time.Now()
	y := now.AddDate(0, 0, -1)

	l.AppendUsage(UsageEntry{Ts: now.Unix(), Ch: "qodercn", Model: "m", PT: 900, CT: 100})
	l.AppendUsage(UsageEntry{Ts: now.Unix(), Ch: "qodercn", Model: "m", PT: 900, CT: 50})
	l.AppendUsage(UsageEntry{Ts: y.Unix(), Ch: "qodercn", Model: "m", PT: 10, CT: 7})
	// CT=0（无 usage 的请求）不贡献输出量，但会被扫描到
	l.AppendUsage(UsageEntry{Ts: now.Unix(), Ch: "qodercn", Model: "m", PT: 0, CT: 0, Src: "none"})
	// 窗口外（30 天前）不该出现
	l.AppendUsage(UsageEntry{Ts: now.AddDate(0, 0, -30).Unix(), Ch: "qodercn", Model: "m", PT: 1, CT: 999})
	// 只有零输出的那一天（3 天前）不该凭空多出一个 0 键
	l.AppendUsage(UsageEntry{Ts: now.AddDate(0, 0, -3).Unix(), Ch: "qodercn", Model: "m", PT: 1200, CT: 0, Src: "none"})

	got := l.DailyCompletion(7)
	if got[now.Format("2006-01-02")] != 150 {
		t.Fatalf("今日输出: got %d want 150", got[now.Format("2006-01-02")])
	}
	if got[y.Format("2006-01-02")] != 7 {
		t.Fatalf("昨日输出: got %d want 7", got[y.Format("2006-01-02")])
	}
	if _, ok := got[now.AddDate(0, 0, -30).Format("2006-01-02")]; ok {
		t.Fatalf("窗口外日期不该出现: %v", got)
	}
	if _, ok := got[now.AddDate(0, 0, -3).Format("2006-01-02")]; ok {
		t.Fatalf("只有零输出的日子不该产生键: %v", got)
	}
	for day, v := range got {
		if v <= 0 {
			t.Fatalf("不应产生零值/负值天: %s=%d", day, v)
		}
	}
}

func TestDailyCompletionEmptyWhenNoUsage(t *testing.T) {
	l := newTestLedger(t)
	if got := l.DailyCompletion(7); len(got) != 0 {
		t.Fatalf("无流水应返回空: %v", got)
	}
	if got := l.DailyCompletion(0); len(got) != 0 {
		t.Fatalf("days=0 应返回空: %v", got)
	}
}
