package stats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// R66：Token 输入/输出拆分。
//
// 背景：面板只显示 Token 合计时，长会话会让用户以为「模型很费 token」，实际是每轮
// 把历史上下文重发一遍（prompt 占绝大部分）。这组测试锁住三件事：
//  1. 合计不变（老口径不能被拆分改坏）；
//  2. 拆分之和恒等于合计；
//  3. 跨天归档、重启恢复、窗口计算都不丢输出量，且未来日期键不被算进窗口。

func TestAddUsageSplitsInOut(t *testing.T) {
	s := newTestStats(t)
	now := tNow(1, 10)
	s.AddUsage("m1", 900, 100, 0, 0, 0, now)
	s.AddUsage("m1", 400, 50, 0, 0, 0, now)

	snap := s.Snapshot(now)
	if snap.Tokens != 1450 {
		t.Fatalf("合计口径被改坏: got %d want 1450", snap.Tokens)
	}
	if snap.TokenUsage.OutToday != 150 {
		t.Fatalf("输出合计错误: got %d want 150", snap.TokenUsage.OutToday)
	}
	if snap.TokenUsage.InToday != 1300 {
		t.Fatalf("输入应为合计−输出: got %d want 1300", snap.TokenUsage.InToday)
	}
	if snap.TokenUsage.InToday+snap.TokenUsage.OutToday != snap.Tokens {
		t.Fatalf("拆分不闭合: in+out=%d 合计=%d",
			snap.TokenUsage.InToday+snap.TokenUsage.OutToday, snap.Tokens)
	}
	if len(snap.Models) != 1 {
		t.Fatalf("模型行数: got %d", len(snap.Models))
	}
	m := snap.Models[0]
	if m.In != 1300 || m.Out != 150 || m.Tokens != 1450 {
		t.Fatalf("模型行拆分错误: in=%d out=%d total=%d", m.In, m.Out, m.Tokens)
	}
	if m.In+m.Out != m.Tokens {
		t.Fatalf("模型行拆分不闭合: %d+%d != %d", m.In, m.Out, m.Tokens)
	}
}

func TestSplitSurvivesDayRoll(t *testing.T) {
	s := newTestStats(t)
	day1 := tNow(1, 10)
	s.AddUsage("m1", 700, 300, 0, 0, 0, day1)

	// 跨天：今日清零、输出量进入 outDays
	day2 := tNow(2, 1)
	s.AddUsage("m1", 10, 5, 0, 0, 0, day2)

	if s.outDays["2026-10-01"] != 300 {
		t.Fatalf("跨天未归档输出量: outDays=%v", s.outDays)
	}
	snap := s.Snapshot(day2)
	if snap.TokenUsage.OutToday != 5 || snap.TokenUsage.Out7 != 305 {
		t.Fatalf("窗口输出错误: outToday=%d out7=%d want 5/305",
			snap.TokenUsage.OutToday, snap.TokenUsage.Out7)
	}
	if snap.TokenUsage.Days7 != 1015 {
		t.Fatalf("窗口合计被改坏: got %d want 1015", snap.TokenUsage.Days7)
	}
	if snap.TokenUsage.In7+snap.TokenUsage.Out7 != snap.TokenUsage.Days7 {
		t.Fatalf("窗口拆分不闭合: %d+%d != %d",
			snap.TokenUsage.In7, snap.TokenUsage.Out7, snap.TokenUsage.Days7)
	}
}

// 未来日期键（时钟回拨/多机写入）不得算进窗口——windowOutLocked 与 windowLocked 同护栏。
func TestWindowOutExcludesFutureDays(t *testing.T) {
	s := newTestStats(t)
	now := tNow(10, 12)
	s.outDays["2026-10-05"] = 40
	s.outDays["2026-10-09"] = 20
	s.outDays["2026-11-20"] = 9999 // 未来键

	if got := s.windowOutLocked(now, 7); got != 60 {
		t.Fatalf("7 日窗输出: got %d want 60（未来键必须排除）", got)
	}
	if got := s.windowOutLocked(now, 30); got != 60 {
		t.Fatalf("30 日窗输出: got %d want 60", got)
	}
}

// 平台表（今日情况主视图）也必须能拿到输出量：按模型名前缀聚合，与 Tokens 同源闭合。
func TestPlatformRowTokensOut(t *testing.T) {
	s := newTestStats(t)
	now := tNow(1, 10)
	s.AddUsage("qodercn/deepseek-flash", 900, 100, 0, 0, 0, now)
	s.AddUsage("qodercn/qwen3.8-flash", 400, 50, 0, 0, 0, now)
	s.AddUsage("raccoon/sn-x", 300, 30, 0, 0, 0, now)

	snap := s.Snapshot(now)
	var qoder *PlatformRow
	var sumTokens, sumOut int64
	for i := range snap.Platforms {
		r := &snap.Platforms[i]
		sumTokens += r.Tokens
		sumOut += r.TokensOut
		if r.Group == "qodercn" {
			qoder = r
		}
	}
	if qoder == nil {
		t.Fatalf("缺少 qodercn 平台行: %+v", snap.Platforms)
	}
	if qoder.Tokens != 1450 || qoder.TokensOut != 150 {
		t.Fatalf("平台行输出错误: tokens=%d out=%d want 1450/150", qoder.Tokens, qoder.TokensOut)
	}
	// 各平台行之和必须等于全池口径（合计行取 s.tokens / out_today，前端两处互证）
	if sumOut != snap.TokenUsage.OutToday {
		t.Fatalf("平台行输出之和 %d != 全池输出 %d", sumOut, snap.TokenUsage.OutToday)
	}
	if sumTokens != snap.Tokens {
		t.Fatalf("平台行合计 %d != 全池合计 %d", sumTokens, snap.Tokens)
	}
}

func TestSplitPersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	// 必须用「真实的今天」：load 里 TodayModels 有 day 守卫（跨天残留整块丢弃），
	// 用固定过去日期会让恢复被正确丢弃，测出假失败。
	now := time.Now()
	s, err := New(dir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.AddUsage("m1", 800, 200, 0, 0, 0, now)
	// 模拟「昨天已归档的输出量」：out_days 若不落盘，重启后 7 日窗口的输出会凭空少一截。
	s.outDays[now.AddDate(0, 0, -1).Format("2006-01-02")] = 500
	s.FlushNow()

	// 原始 JSON 里必须有拆分字段（否则重启就只剩合计，拆分列永远 "—"）
	raw, err := os.ReadFile(filepath.Join(dir, "stats.json"))
	if err != nil {
		t.Fatalf("读 stats.json: %v", err)
	}
	var probe struct {
		OutDays     map[string]int64 `json:"out_days"`
		TodayModels *struct {
			Models map[string]struct {
				In  int64 `json:"in_tokens"`
				Out int64 `json:"out_tokens"`
			} `json:"models"`
		} `json:"today_models"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("解析 stats.json: %v", err)
	}
	if probe.TodayModels == nil || probe.TodayModels.Models["m1"].Out != 200 {
		t.Fatalf("今日模型拆分未落盘: %s", string(raw))
	}

	s2, err := New(dir, nil)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	snap := s2.Snapshot(now)
	if snap.TokenUsage.OutToday != 200 {
		t.Fatalf("重启后输出量丢失: got %d want 200", snap.TokenUsage.OutToday)
	}
	if snap.TokenUsage.Out7 != 700 {
		t.Fatalf("重启后历史输出窗口丢失: got %d want 700（今日 200 + 昨日归档 500）", snap.TokenUsage.Out7)
	}
	if snap.Tokens != 1000 {
		t.Fatalf("重启后合计丢失: got %d want 1000", snap.Tokens)
	}
	if len(snap.Models) != 1 || snap.Models[0].In != 800 || snap.Models[0].Out != 200 {
		t.Fatalf("重启后模型拆分丢失: %+v", snap.Models)
	}
}

// 旧 stats.json（无 out_days / 无拆分字段）必须还能读：只少了拆分，合计不受影响。
func TestLegacyStatsJSONWithoutSplit(t *testing.T) {
	dir := t.TempDir()
	today := time.Now().Format("2006-01-02")
	legacy := `{"token_days":{"` + today + `":500},"req_days":{"` + today + `":2},"accts":{}}`
	if err := os.WriteFile(filepath.Join(dir, "stats.json"), []byte(legacy), 0o644); err != nil {
		t.Fatalf("写旧格式: %v", err)
	}
	s, err := New(dir, nil)
	if err != nil {
		t.Fatalf("读旧格式失败: %v", err)
	}
	snap := s.Snapshot(time.Now())
	if snap.TokenUsage.Days7 != 500 {
		t.Fatalf("旧格式合计应保留: got %d want 500", snap.TokenUsage.Days7)
	}
	if snap.TokenUsage.Out7 != 0 || snap.TokenUsage.In7 != 500 {
		t.Fatalf("旧格式无拆分时输出应为 0、输入=合计: out7=%d in7=%d",
			snap.TokenUsage.Out7, snap.TokenUsage.In7)
	}
}
