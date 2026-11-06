package stats

import (
	"testing"
	"time"
)

// R97：临期行的「花费」必须是净值（差值下降扣今日已作废），且作废额按账号
// 维度喂入、跨天清零、全量替换。回归事故：昨领 300 今晨整包作废（一分未用）、
// 白天又领 300 ⇒ creditOut=300 全是作废、真实花费 0，旧口径外推出
// 「已耗 300 · 预计作废 264」自相矛盾。
func TestR97ExpiryRowSplitsSpentAndExpired(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	base := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local)
	expiry := base.AddDate(0, 0, 3).Format("2006-01-02")

	s.ApplyPoll([]Account{{UID: "a", Group: "traework", Credits: 500}}, base)
	s.ApplyPoll([]Account{{UID: "a", Group: "traework", Credits: 200}}, base.Add(time.Minute)) // 差值 300
	s.ApplyExpiry("a", expiry, 1000)
	// 该账号今日作废 300（全部差值都是作废）；另一个账号的作废不许串过来。
	s.ApplyLedgerExpire(todayKey(base), []LedgerExpire{
		{Channel: "traework", UID: "a", Amount: 300},
		{Channel: "traework", UID: "b", Amount: 999},
	})

	row := s.Snapshot(base).Expiry[0]
	if row.TodayOut != 300 {
		t.Fatalf("today_out = %d, want 300（旧口径字段保持原样）", row.TodayOut)
	}
	if row.TodaySpent != 0 {
		t.Fatalf("today_spent = %d, want 0（300 差值全是作废）", row.TodaySpent)
	}
	if row.TodayExpired != 300 {
		t.Fatalf("today_expired = %d, want 300", row.TodayExpired)
	}
	if row.BurnForecast != -1 {
		t.Fatalf("净花费 0 时不得外推：burn_forecast = %d, want -1", row.BurnForecast)
	}
}

// 作废额按 uid 隔离 + 全量替换 + 跨天清零（expiredByUID 与 expiredByGroup 同步）。
func TestR97ExpiredByUIDReplaceAndDayRoll(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	base := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local)
	expiry := base.AddDate(0, 0, 3).Format("2006-01-02")
	s.ApplyPoll([]Account{{UID: "a", Group: "traework", Credits: 500}}, base)
	s.ApplyPoll([]Account{{UID: "a", Group: "traework", Credits: 400}}, base.Add(time.Minute)) // 差值 100
	s.ApplyExpiry("a", expiry, 1000)

	// 首轮：a 作废 40 ⇒ 净花费 60
	s.ApplyLedgerExpire(todayKey(base), []LedgerExpire{
		{Channel: "traework", UID: "a", Amount: 40},
	})
	if row := s.Snapshot(base).Expiry[0]; row.TodaySpent != 60 || row.TodayExpired != 40 {
		t.Fatalf("首轮 spent/expired = %d/%d, want 60/40", row.TodaySpent, row.TodayExpired)
	}
	// 次轮全量替换：只报 a 作废 10 ⇒ 净花费 90（不是 60−10）
	s.ApplyLedgerExpire(todayKey(base), []LedgerExpire{
		{Channel: "traework", UID: "a", Amount: 10},
	})
	if row := s.Snapshot(base).Expiry[0]; row.TodaySpent != 90 || row.TodayExpired != 10 {
		t.Fatalf("全量替换后 spent/expired = %d/%d, want 90/10", row.TodaySpent, row.TodayExpired)
	}
	// 空切片 = 今日无作废 → 清零，净花费回到完整差值 100
	s.ApplyLedgerExpire(todayKey(base), []LedgerExpire{})
	if row := s.Snapshot(base).Expiry[0]; row.TodaySpent != 100 || row.TodayExpired != 0 {
		t.Fatalf("空切片后 spent/expired = %d/%d, want 100/0", row.TodaySpent, row.TodayExpired)
	}
	// 跨天：expiredByUID 必须随 rollDay 清零，不能带着昨天的作废额进新一天
	s.ApplyLedgerExpire(todayKey(base), []LedgerExpire{
		{Channel: "traework", UID: "a", Amount: 70},
	})
	if row := s.Snapshot(base.Add(24 * time.Hour)).Expiry[0]; row.TodayExpired != 0 {
		t.Fatalf("跨天未清零: today_expired = %d, want 0", row.TodayExpired)
	}
}

// 空 UID 只进渠道维度，不进 uid 维度；渠道空值归 unknown。
func TestR97EmptyUIDOnlyCountsGroup(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	base := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local)
	s.ApplyLedgerExpire(todayKey(base), []LedgerExpire{
		{Channel: "", UID: "", Amount: 7},
		{Channel: "traework", UID: "a", Amount: 5},
	})
	snap := s.Snapshot(base)
	if snap.CreditExpired != 12 {
		t.Fatalf("credit_expired = %d, want 12", snap.CreditExpired)
	}
	if len(s.expiredByUID) != 1 || s.expiredByUID["a"] != 5 {
		t.Fatalf("空 UID 不应进 uid 维度: %+v", s.expiredByUID)
	}
}

// 观测充足时，外推速率必须按净花费而非余额差值。
// 回归「整包作废被当成消耗速度」：差值 100 里 40 是作废，净花费 60 ⇒
// 1 小时观测、距到期 83 小时 → forecast 应为 4980；若退回 creditOut 会算成 8300。
func TestR97BurnForecastUsesNetSpent(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	base := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local)
	expiry := base.AddDate(0, 0, 3).Format("2006-01-02")
	s.ApplyPoll([]Account{{UID: "a", Group: "traework", Credits: 200}}, base)
	s.ApplyExpiry("a", expiry, 20000)
	s.ApplyPoll([]Account{{UID: "a", Group: "traework", Credits: 100}}, base.Add(time.Hour)) // 差值 100
	s.ApplyLedgerExpire(todayKey(base), []LedgerExpire{
		{Channel: "traework", UID: "a", Amount: 40},
	})
	row := s.Snapshot(base.Add(time.Hour)).Expiry[0]
	if row.TodayOut != 100 || row.TodaySpent != 60 || row.TodayExpired != 40 {
		t.Fatalf("out/spent/expired = %d/%d/%d, want 100/60/40", row.TodayOut, row.TodaySpent, row.TodayExpired)
	}
	// 60/1h × 83h = 4980
	if row.BurnForecast != 4980 {
		t.Fatalf("burn_forecast = %d, want 4980（按净花费 60 外推，不是差值 100）", row.BurnForecast)
	}
}

// Version 载荷语义版本：新增字段后必须 bump。
func TestR97VersionBumped(t *testing.T) {
	if Version != "2" {
		t.Fatalf("Version = %q, want 2", Version)
	}
}
