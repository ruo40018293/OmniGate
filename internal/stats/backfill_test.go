package stats

// R69：历史输出量回填（out_days 补历史）。
//
// 背景：out_days 是 R66 引入的字段，老数据只有「合计」归档，没有输出归档。
// 不回填时 Out7/Out30 恒等于今日输出（实测 out7 == out_today == 53289），
// 且 In7 = Days7 - Out7 被同步抬高——两个窗口同时在说谎。
//
// 这组测试锁住四条边界：
//  1. 缺失的历史日被填上；2. 今日绝不被流水值覆盖；3. 已有归档不被覆盖；
//  4. 幂等（第二次调用回填 0 天），且窗口立即反映真实历史。

import (
	"testing"
	"time"
)

func dayKey(offset int) string {
	return time.Now().AddDate(0, 0, offset).Format("2006-01-02")
}

func TestBackfillOutFillsPastDays(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()

	// 今日实时计数：prompt 900 / completion 100
	s.AddUsage("m1", 900, 100, 0, 0, 0, now)

	y := dayKey(-1)
	d5 := dayKey(-5)
	n := s.BackfillOut(map[string]int64{
		y:         700,
		dayKey(0): 999, // 今日：必须被忽略（实时计数才权威）
		d5:        40,
	})
	if n != 2 {
		t.Fatalf("回填天数: got %d want 2（今日不算）", n)
	}
	if got := s.outDays[y]; got != 700 {
		t.Fatalf("昨日输出未回填: got %d want 700", got)
	}
	if got := s.outDays[dayKey(0)]; got != 0 {
		t.Fatalf("今日被流水值覆盖: got %d want 0（今日走实时计数）", got)
	}
	if got := s.outDays[d5]; got != 40 {
		t.Fatalf("5 天前输出未回填: got %d want 40", got)
	}

	// 窗口立即变真：今日 100 + 昨日 700 + 5 天前 40
	snap := s.Snapshot(now)
	if got := snap.TokenUsage.Out7; got != 840 {
		t.Fatalf("Out7 应含回填历史: got %d want 840", got)
	}
	if got := snap.TokenUsage.OutToday; got != 100 {
		t.Fatalf("OutToday 应只含实时: got %d want 100", got)
	}
	// 输入 = 合计 − 输出（合计只有今日 1000，窗口内无历史合计数据）
	if got, want := snap.TokenUsage.In7, snap.TokenUsage.Days7-snap.TokenUsage.Out7; got != want {
		t.Fatalf("In7 应与合计−输出同源: got %d want %d", got, want)
	}
}

func TestBackfillOutKeepsExistingArchive(t *testing.T) {
	s := newTestStats(t)
	y := dayKey(-1)
	s.outDays[y] = 500 // 已有磁盘归档

	n := s.BackfillOut(map[string]int64{y: 700, dayKey(-2): 33})
	if n != 1 {
		t.Fatalf("回填天数: got %d want 1（已有归档的那个跳过）", n)
	}
	if got := s.outDays[y]; got != 500 {
		t.Fatalf("既有归档被流水覆盖: got %d want 500", got)
	}
	if got := s.outDays[dayKey(-2)]; got != 33 {
		t.Fatalf("缺失日未回填: got %d want 33", got)
	}
}

func TestBackfillOutIdempotent(t *testing.T) {
	s := newTestStats(t)
	daily := map[string]int64{dayKey(-1): 700, dayKey(-3): 12}

	if n := s.BackfillOut(daily); n != 2 {
		t.Fatalf("首次回填: got %d want 2", n)
	}
	if n := s.BackfillOut(daily); n != 0 {
		t.Fatalf("二次回填应幂等: got %d want 0", n)
	}
	if n := s.BackfillOut(nil); n != 0 {
		t.Fatalf("空输入应直接返回 0: got %d", n)
	}
}

func TestBackfillOutSurvivesReload(t *testing.T) {
	s := newTestStats(t)
	y := dayKey(-1)
	s.BackfillOut(map[string]int64{y: 700})
	s.FlushNow()

	// 重新从 stats.json 载入：回填结果必须持久化（否则每次重启重扫，且中途
	// 一旦流水被 trimCurrentMonth 裁掉就永久丢失）。
	s2, err := New(s.dir, nil)
	if err != nil {
		t.Fatalf("New reload: %v", err)
	}
	if got := s2.outDays[y]; got != 700 {
		t.Fatalf("回填结果未落盘: got %d want 700", got)
	}
}
