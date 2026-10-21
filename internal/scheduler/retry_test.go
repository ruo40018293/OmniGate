package scheduler

// retry_test.go 限流延迟重试的回归测试。
//
// 锁定的事实：
//  1. 退避递增（10/20/30…分钟）且跨天重置计数；
//  2. 每天上限 rateLimitRetryMax —— 防止上游长期异常时无限打上游；
//  3. 成功即清除状态（否则第二天会带着昨天的退避计数继续累加）；
//  4. 开关默认关闭，关闭时**完全不产生重试**（这是渠道级的显式选择）；
//  5. 「余额查询失败」不触发重试 —— 签到可能已经入账了，只是查不到余额；
//  6. nextRetryAt 能把 Run 循环的唤醒点提前到重试时刻。

import (
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"omnigate/internal/auth"
	"omnigate/internal/pool"
	"omnigate/internal/provider"
)

// retryUpstream 是一个不与真实网络打交道的假上游：签到恒成功、余额固定。
// 本文件的测试只关心「重试状态机」，不需要真实的签到/刷新行为。
type retryUpstream struct {
	remain int64
}

func newRetryUpstream(remain int64) *retryUpstream { return &retryUpstream{remain: remain} }

func (u *retryUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}
func (u *retryUpstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return u.remain, []provider.ResourceItem{{Name: "套餐", Remain: u.remain, Usable: true}}, nil
}
func (u *retryUpstream) RefreshToken(*auth.Auth) error { return nil }
func (u *retryUpstream) DailyCheckin(*auth.Auth) error { return nil }
func (u *retryUpstream) ChatStream(*auth.Auth, []byte) (io.ReadCloser, int, []byte, error) {
	return nil, 0, nil, nil
}
func (u *retryUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) {
	return nil, nil
}
func (u *retryUpstream) UserResource(*auth.Auth) (int64, error) { return u.remain, nil }
func (u *retryUpstream) Classify(int, string) provider.ErrKind  { return provider.ErrNone }
func (u *retryUpstream) Stream(http.ResponseWriter, io.Reader, string) (map[string]any, error) {
	return nil, nil
}
func (u *retryUpstream) Aggregate(io.Reader, string) (map[string]any, error) {
	return map[string]any{}, nil
}

// newTestPool 建一个内存池。
func newTestPool(t *testing.T) *pool.Pool {
	t.Helper()
	return pool.New("")
}

// newTestAuth 建一个带 refresh token 的凭据（checkinOne 需要它才不发 no_token）。
func newTestAuth(uid string) *auth.Auth {
	return &auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 4102444800}
}

// newRetrySched 造一个开启了延迟重试、含 n 个账号的调度器。
func newRetrySched(t *testing.T, n int) *Scheduler {
	t.Helper()
	p := newTestPool(t)
	for i := 0; i < n; i++ {
		p.Add(newTestAuth(uidFor(i)))
	}
	return New(Config{Pool: p, Upstream: newRetryUpstream(1000), Name: "traework", RetryOnFailure: true})
}

func uidFor(i int) string { return []string{"u1", "u2", "u3"}[i] }

// TestMarkRetryableBackoffIncrements 退避按次数线性递增：10min → 20min → 30min。
//
// 为什么是线性而非指数：指数退避第 3 次就是 40 分钟、第 4 次 80 分钟，
// 那样「尽快补领」就失去意义了（用户当天没领到就是没领到）。
func TestMarkRetryableBackoffIncrements(t *testing.T) {
	s := newRetrySched(t, 1)
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.Local)

	for i := 1; i <= 4; i++ {
		at, ok := s.markRetryable("u1", now, 9*60)
		if !ok {
			t.Fatalf("第%d次安排失败", i)
		}
		want := now.Add(rateLimitRetryDelay * time.Duration(i))
		if !at.Equal(want) {
			t.Fatalf("第%d次重试时刻=%v，want %v", i, at, want)
		}
	}
}

// TestMarkRetryableCapsPerDay 单账号每天最多 rateLimitRetryMax 次。
func TestMarkRetryableCapsPerDay(t *testing.T) {
	s := newRetrySched(t, 1)
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.Local)

	for i := 0; i < rateLimitRetryMax; i++ {
		if _, ok := s.markRetryable("u1", now, 9*60); !ok {
			t.Fatalf("第%d次应能安排（上限=%d）", i+1, rateLimitRetryMax)
		}
	}
	at, ok := s.markRetryable("u1", now, 9*60)
	if ok {
		t.Fatalf("超过上限(%d)后仍安排了重试：%v", rateLimitRetryMax, at)
	}
	// 计数不应继续增长（否则跨天后会误以为已经用尽）
	s.retryMu.Lock()
	cnt := s.retries["u1"].count
	s.retryMu.Unlock()
	if cnt != rateLimitRetryMax {
		t.Fatalf("计数=%d，应停在上限 %d", cnt, rateLimitRetryMax)
	}
}

// TestMarkRetryableResetsNextDay 跨天计数重置 —— 限流是当天的事，
// 不该因为昨天用尽 6 次就让今天彻底不重试。
func TestMarkRetryableResetsNextDay(t *testing.T) {
	s := newRetrySched(t, 1)
	day1 := time.Date(2026, 10, 12, 9, 0, 0, 0, time.Local)
	for i := 0; i < rateLimitRetryMax; i++ {
		s.markRetryable("u1", day1, 9*60)
	}
	if _, ok := s.markRetryable("u1", day1, 9*60); ok {
		t.Fatal("当天用尽后应拒绝")
	}

	day2 := day1.Add(24 * time.Hour) // 次日同一时刻
	if _, ok := s.markRetryable("u1", day2, 9*60); !ok {
		t.Fatal("跨天后应重新允许安排")
	}
	s.retryMu.Lock()
	cnt := s.retries["u1"].count
	s.retryMu.Unlock()
	if cnt != 1 {
		t.Fatalf("跨天后计数=%d，应重置为 1", cnt)
	}
}

// TestDueRetriesClearsPending 到期取出后清 pending，重复调用不会重复触发。
func TestDueRetriesClearsPending(t *testing.T) {
	s := newRetrySched(t, 2)
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.Local)
	s.markRetryable("u1", now, 9*60)
	s.markRetryable("u2", now, 9*60)

	// 未到期：取不到
	if got := s.dueRetries(now.Add(9 * time.Minute)); len(got) != 0 {
		t.Fatalf("未到期却取到 %v", got)
	}
	// 到期：两个都应被取出
	got := s.dueRetries(now.Add(10 * time.Minute))
	if len(got) != 2 {
		t.Fatalf("到期应取出 2 个，实际 %v", got)
	}
	// 再取一次：pending 已清，应为空（否则同一个账号会被反复重试）
	if again := s.dueRetries(now.Add(10 * time.Minute)); len(again) != 0 {
		t.Fatalf("pending 未清除，重复取出 %v", again)
	}
}

// TestClearRetryOnSuccess 成功后清除状态。
func TestClearRetryOnSuccess(t *testing.T) {
	s := newRetrySched(t, 1)
	now := time.Now()
	s.markRetryable("u1", now, 9*60)
	if s.pendingRetryCount() != 1 {
		t.Fatalf("安排后待执行数=%d", s.pendingRetryCount())
	}
	s.clearRetry("u1")
	if s.pendingRetryCount() != 0 {
		t.Fatalf("清除后待执行数=%d", s.pendingRetryCount())
	}
	if !s.nextRetryAt().IsZero() {
		t.Fatal("清除后 nextRetryAt 应为零值（否则 Run 循环会被无意义地提前唤醒）")
	}
}

// TestNextRetryAtPicksEarliest nextRetryAt 返回最早的待执行时刻。
func TestNextRetryAtPicksEarliest(t *testing.T) {
	s := newRetrySched(t, 2)
	now := time.Now()
	s.markRetryable("u1", now, 9*60)
	s.markRetryable("u2", now.Add(-5*time.Minute), 9*60) // u2 排得更早（第二次退避前… 用过去时间模拟更早的排期）

	got := s.nextRetryAt()
	want := now.Add(-5*time.Minute + rateLimitRetryDelay)
	if !got.Equal(want) {
		t.Fatalf("nextRetryAt=%v，want %v", got, want)
	}
}

// TestScheduleRetryForClearsOnSuccess 成功签到必须清除退避状态。
//
// 不清除的后果：某账号白天成功、晚上又失败，退避计数会从白天的值继续累加，
// 几天后即使限流已恢复也排不出重试。
func TestScheduleRetryForClearsOnSuccess(t *testing.T) {
	s := newRetrySched(t, 1)
	s.markRetryable("u1", time.Now(), -1)
	if s.pendingRetryCount() != 1 {
		t.Fatal("前置：应已有待执行重试")
	}
	s.scheduleRetryFor("u1", CheckinResult{UID: "u1", OK: true}, time.Now(), -1)
	if s.pendingRetryCount() != 0 {
		t.Fatalf("成功后未清除，待执行数=%d", s.pendingRetryCount())
	}
}

// TestScheduleRetryForArrangesOnRetryable 可重试失败应安排（续排）延迟重试。
func TestScheduleRetryForArrangesOnRetryable(t *testing.T) {
	s := newRetrySched(t, 1)
	s.scheduleRetryFor("u1", CheckinResult{UID: "u1", OK: false, Retryable: true, Msg: "上游繁忙"}, time.Now(), -1)
	if s.pendingRetryCount() != 1 {
		t.Fatalf("可重试失败未安排重试，待执行数=%d", s.pendingRetryCount())
	}
	// 续排应递增而不是覆盖：再次可重试失败后时间点应推后
	first := s.nextRetryAt()
	s.scheduleRetryFor("u1", CheckinResult{UID: "u1", OK: false, Retryable: true}, time.Now(), -1)
	if second := s.nextRetryAt(); !second.After(first) {
		t.Fatalf("续排未递增退避：%v → %v", first, second)
	}
}

// TestScheduleRetryForIgnoresNonRetryableFailure 不可重试的失败（session dead 等）
// 不该排重试 —— 那类问题重试多少次都不会好，只会白白打上游。
func TestScheduleRetryForIgnoresNonRetryableFailure(t *testing.T) {
	s := newRetrySched(t, 1)
	s.scheduleRetryFor("u1", CheckinResult{UID: "u1", OK: false, Retryable: false, Msg: "session dead"}, time.Now(), -1)
	if s.pendingRetryCount() != 0 {
		t.Fatalf("不可重试失败不应安排重试，待执行数=%d", s.pendingRetryCount())
	}
}

// TestRetryDisabledByDefaultAndNoOps 渠道未开启时，scheduleRetryFor 不做任何事。
//
// 这是「显式选择」的关键：Qoder 的 no_campaign / no refresh token 也会置
// Retryable，但对它们排延迟重试是无意义的（前者已由窗口重试覆盖）。
func TestRetryDisabledByDefaultAndNoOps(t *testing.T) {
	s := New(Config{Pool: newTestPool(t), Upstream: newRetryUpstream(1000), Name: "qodercn"})
	if s.retryOnFailureEnabled() {
		t.Fatal("默认应为关闭")
	}
	s.scheduleRetryFor("u1", CheckinResult{UID: "u1", OK: false, Retryable: true}, time.Now(), -1)
	if s.pendingRetryCount() != 0 {
		t.Fatalf("关闭状态下不应产生重试，待执行数=%d", s.pendingRetryCount())
	}
	// runDueRetries 也必须空转（否则每分钟都会白跑一次）
	s.runDueRetries(time.Now().Add(24 * time.Hour))
	if s.pendingRetryCount() != 0 {
		t.Fatal("关闭状态下 runDueRetries 不应产生重试")
	}
}

// TestSetRetryOnFailureToggle 开关的读取与切换自洽。
func TestSetRetryOnFailureToggle(t *testing.T) {
	s := newRetrySched(t, 1)
	if !s.retryOnFailureEnabled() {
		t.Fatal("构造时应为开启")
	}
	s.setRetryOnFailure(false)
	if s.retryOnFailureEnabled() {
		t.Fatal("关闭失败")
	}
	s.scheduleRetryFor("u1", CheckinResult{UID: "u1", Retryable: true}, time.Now(), -1)
	if s.pendingRetryCount() != 0 {
		t.Fatal("关闭后不应产生新重试")
	}
	s.setRetryOnFailure(true)
	s.scheduleRetryFor("u1", CheckinResult{UID: "u1", Retryable: true}, time.Now(), -1)
	if s.pendingRetryCount() != 1 {
		t.Fatal("重新开启后应恢复安排重试")
	}
}

// TestRetryStateIsPerAccount 不同账号各自计数，互不影响。
//
// 关键场景：两个账号同时限流，其中一个恢复得快，不该把另一个的名额也吃掉。
func TestRetryStateIsPerAccount(t *testing.T) {
	s := newRetrySched(t, 3)
	now := time.Now()
	for i := 0; i < rateLimitRetryMax; i++ {
		s.markRetryable("u1", now, 9*60)
	}
	if _, ok := s.markRetryable("u2", now, 9*60); !ok {
		t.Fatal("u1 用尽不应影响 u2")
	}
	s.clearRetry("u1")
	if s.pendingRetryCount() != 1 {
		t.Fatalf("清除 u1 后应只剩 u2 待重试，实际 %d", s.pendingRetryCount())
	}
	if _, ok := s.markRetryable("u1", now, 9*60); !ok {
		t.Fatal("u1 清除后应能重新安排")
	}
}

// TestRunDueRetriesSkipsRemovedAccount 账号已从池中移除时应清理状态，
// 不再对它发起请求（否则每次唤醒都会对着一个不存在的账号重试）。
func TestRunDueRetriesSkipsRemovedAccount(t *testing.T) {
	s := newRetrySched(t, 1)
	now := time.Now()
	s.markRetryable("u1", now, 9*60)
	s.cfg.Pool.Remove("u1")

	s.runDueRetries(now.Add(rateLimitRetryDelay))
	if s.pendingRetryCount() != 0 {
		t.Fatalf("账号已移除但状态未清理，待执行数=%d", s.pendingRetryCount())
	}
}

// balanceFailUpstream 签到成功但查余额失败。
//
// 用来锁定「余额查询失败不该触发延迟重试」：签到可能**已经入账**了，
// 只是查不到余额。此时若按 r.OK=false 去重试，就是对着已领到的账号反复打上游，
// 严重的会一路重试到当天上限。
type balanceFailUpstream struct{ retryUpstream }

func (u *balanceFailUpstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, errors.New("余额查询超时")
}

// TestCheckinOneDoesNotRetryWhenOnlyBalanceQueryFails 签到成功 + 余额查询失败
// → 不安排延迟重试，且要清掉此前积压的重试状态。
func TestCheckinOneDoesNotRetryWhenOnlyBalanceQueryFails(t *testing.T) {
	p := newTestPool(t)
	p.Add(newTestAuth("u1"))
	s := New(Config{Pool: p, Upstream: &balanceFailUpstream{}, Name: "traework", RetryOnFailure: true})

	// 先造一个积压的重试状态（模拟此前因限流排了重试）
	s.markRetryable("u1", time.Now(), -1)
	if s.pendingRetryCount() != 1 {
		t.Fatal("前置：应已有待执行重试")
	}

	r := s.checkinOne("u1", -1)
	if r.OK {
		t.Fatal("余额查询失败时整体应报失败（供面板显示）")
	}
	if s.pendingRetryCount() != 0 {
		t.Fatalf("签到已成功，不应因余额查询失败而安排/保留重试，待执行数=%d", s.pendingRetryCount())
	}
}

// TestRunDueRetriesActuallyRecovers 端到端：撞限流 → 排重试 → 到期重试签到成功 →
// 状态清除。这条锁的是整个机制的「闭环」，前面各例只覆盖单步。
func TestRunDueRetriesActuallyRecovers(t *testing.T) {
	// 第一次 error（可重试），第二次 claimed（成功）
	up := &reporterUpstream{statuses: []provider.CheckinStatus{provider.CheckinError, provider.CheckinClaimed}}
	p := newTestPool(t)
	p.Add(newTestAuth("u1"))
	s := New(Config{Pool: p, Upstream: up, Name: "traework", RetryOnFailure: true,
		CheckinMinutes: []int{9 * 60}})

	// 模拟 09:00 的定时签到：撞上游错误
	s.RunCheckinNow()
	if s.pendingRetryCount() != 1 {
		t.Fatalf("撞限流后应排一次重试，待执行数=%d", s.pendingRetryCount())
	}
	callsAfterFirst := up.calls.Load()

	// 到期重试：应签到成功并清空状态
	s.runDueRetries(time.Now().Add(rateLimitRetryDelay))
	if up.calls.Load() <= callsAfterFirst {
		t.Fatalf("到期重试未真正发起签到（calls %d → %d）", callsAfterFirst, up.calls.Load())
	}
	if s.pendingRetryCount() != 0 {
		t.Fatalf("重试成功后应清除状态，待执行数=%d", s.pendingRetryCount())
	}
}

// TestRunLoopWakesForPendingRetry Run 循环必须被「待执行重试」提前唤醒。
//
// 这是整个机制的命门：不提前唤醒的话，延迟重试仍要等到下一个定时点
// （默认 21:00），等于没做。本例直接验证唤醒点被算成了重试时刻。
func TestRunLoopWakesForPendingRetry(t *testing.T) {
	p := newTestPool(t)
	p.Add(newTestAuth("u1"))
	s := New(Config{Pool: p, Upstream: &retryUpstream{remain: 1000}, Name: "traework", RetryOnFailure: true})

	// 安排一个 10 分钟后的重试，且下一个定时签到点在很久以后
	// （用「当前时间 + 23:00」确保重试时刻一定早于定时点）。
	s.markRetryable("u1", time.Now(), -1)
	retryAt := s.nextRetryAt()

	ch, _ := s.schedule()
	next := nextFireMinutes(time.Now(), s.fireMinutes(ch))
	if !retryAt.Before(next) {
		t.Skipf("本机时钟下定时点(%v)早于重试点(%v)，跳过", next, retryAt)
	}

	// 复刻 Run 循环里的唤醒点计算（见 Run 内的同名逻辑）
	wake := next
	if ra := s.nextRetryAt(); !ra.IsZero() && ra.After(time.Now()) && ra.Before(wake) {
		wake = ra
	}
	if !wake.Equal(retryAt) {
		t.Fatalf("唤醒点=%v，应提前到重试时刻 %v", wake, retryAt)
	}
}

// TestRetryDoesNotAdvanceNextScheduledSlot 延迟重试**不得**影响
// 「本时段当日是否已完成」的判定 —— 否则 09:00 的一次延迟重试成功
// 会把 21:00 那个时段也一并吞掉。
//
// 回归背景：旧项目没有「时段」概念（每天只跑一次），照搬会破坏本项目的多时段模型。
func TestRetryDoesNotAdvanceNextScheduledSlot(t *testing.T) {
	up := &reporterUpstream{statuses: []provider.CheckinStatus{provider.CheckinError, provider.CheckinClaimed}}
	p := newTestPool(t)
	p.Add(newTestAuth("u1"))
	s := New(Config{Pool: p, Upstream: up, Name: "traework", RetryOnFailure: true,
		CheckinMinutes: []int{9 * 60, 21 * 60}})

	// 09:00 时段失败 → 该时段未完成
	s.runCheckin(9 * 60)
	if s.checkinSlotDone(9 * 60) {
		t.Fatal("签到失败时不应标记该时段已完成")
	}
	// 延迟重试成功 → 只影响 09:00 时段
	s.runDueRetries(time.Now().Add(rateLimitRetryDelay))
	if !s.checkinSlotDone(9 * 60) {
		t.Fatal("重试成功后应标记 09:00 时段已完成")
	}
	if s.checkinSlotDone(21 * 60) {
		t.Fatal("延迟重试不得顺带标记 21:00 时段完成（会吞掉晚间签到）")
	}
}

// TestRetryCrossingMidnightDoesNotMarkNextDaySlot 跨零点的延迟重试
// **不得**回填时段完成标记。
//
// 回归场景：签到退避最长累计 10+20+…+60 分钟。若 23:50 的签到失败、
// 跨零点后才补领成功，markSlotDone 用 time.Now() 拼 key 会把
// **第二天**同一时刻标成「当日完成」—— 第二天到点时 fireMinutes 跳过该时段，
// 整整漏签一天且日志里毫无异常。
func TestRetryCrossingMidnightDoesNotMarkNextDaySlot(t *testing.T) {
	p := newTestPool(t)
	p.Add(newTestAuth("u1"))
	s := New(Config{Pool: p, Upstream: newRetryUpstream(1000), Name: "traework", RetryOnFailure: true})

	// 造一个「昨天排期、今天到期」的重试（lastDay ≠ 当天）
	now := time.Now()
	yesterday := now.AddDate(0, 0, -1).YearDay()
	s.retryMu.Lock()
	s.retries["u1"] = &retryState{count: 1, lastDay: yesterday, pending: true, slot: 21 * 60}
	s.retryMu.Unlock()

	slot, sameDay := s.retryTarget("u1", now)
	if slot != 21*60 {
		t.Fatalf("slot=%d，应为 21:00", slot)
	}
	if sameDay {
		t.Fatal("lastDay=昨天，应判定为跨零点")
	}

	// 端到端：跑一轮到期重试（凭据有效 → 签到成功），次日时段不得被标记
	s.runDueRetries(now.Add(rateLimitRetryDelay))
	if s.checkinSlotDone(21 * 60) {
		t.Fatal("跨零点的重试把当天 21:00 误标为已完成 —— 明天会漏签一整天")
	}
}

// TestRetrySameDayMarksSlot 同一天内的延迟重试成功后**应当**回填时段标记
// （与上一条互为对照，确保闸门没把正常路径也堵掉）。
func TestRetrySameDayMarksSlot(t *testing.T) {
	up := &reporterUpstream{statuses: []provider.CheckinStatus{provider.CheckinError, provider.CheckinClaimed}}
	p := newTestPool(t)
	p.Add(newTestAuth("u1"))
	s := New(Config{Pool: p, Upstream: up, Name: "traework", RetryOnFailure: true,
		CheckinMinutes: []int{9 * 60, 21 * 60}})
	s.runCheckin(9 * 60)
	s.runDueRetries(time.Now().Add(rateLimitRetryDelay))
	if !s.checkinSlotDone(9 * 60) {
		t.Fatal("同一天内重试成功后应回填 09:00 时段标记")
	}
}
