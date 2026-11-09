package scheduler

// catchup_test.go R99「今日漏跑就补跑」的闸门。
//
// 最重要的一条：**不能在正常路径上多打一次上游**。
// 补跑是兜底，一旦判定条件写松（比如漏掉「今天已经成功过」这一条），
// 每次定时器唤醒都会补跑一次，签到就变成每分钟打一遍上游。

import (
	"sync/atomic"
	"testing"
	"time"

	"omnigate/internal/auth"
	"omnigate/internal/pool"
	"omnigate/internal/provider"
)

// catchupUpstream 记录签到调用次数，并可控返回结果。
type catchupUpstream struct {
	provider.Upstream
	calls    atomic.Int32
	statuses []provider.CheckinStatus
}

func (c *catchupUpstream) next() provider.CheckinReport {
	i := int(c.calls.Add(1)) - 1
	if i >= len(c.statuses) {
		i = len(c.statuses) - 1
	}
	st := c.statuses[i]
	return provider.CheckinReport{Status: st, Msg: string(st), Amount: 300}
}

func (c *catchupUpstream) DailyCheckin(a *auth.Auth) error {
	_, err := c.DailyCheckinReport(a)
	return err
}

func (c *catchupUpstream) DailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {
	return c.next(), nil
}

func (c *catchupUpstream) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	return 500, nil, nil
}

func (c *catchupUpstream) RefreshToken(a *auth.Auth) error { return nil }

// catchupScheduler 构造一个 09:00 / 21:00 签到的调度器（1 账号）。
func catchupScheduler(up provider.Upstream) (*Scheduler, *pool.Pool) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	return New(Config{
		Pool: p, Upstream: up, Name: "workbuddy",
		CheckinMinutes: []int{9 * 60, 21 * 60},
	}), p
}

// at 构造指定当天时刻的时间点。
func at(hour, minute int) time.Time {
	n := time.Now()
	return time.Date(n.Year(), n.Month(), n.Day(), hour, minute, 0, 0, n.Location())
}

// ★ 核心：进程在 09:00 之后启动、今天没有成功记录 ⇒ 必须补跑一次。
func TestCatchUpCheckinRunsWhenTodayNeverSucceeded(t *testing.T) {
	up := &catchupUpstream{statuses: []provider.CheckinStatus{provider.CheckinClaimed}}
	s, _ := catchupScheduler(up)
	// 新导入账号：LastCheckinAt 为零值。
	s.catchUpCheckin(at(14, 0))

	// 补跑是异步的（safeGo），等它把调用打出去。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if up.calls.Load() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := up.calls.Load(); n == 0 {
		t.Fatal("今日无成功签到记录且已过首个时刻，应补跑，实际 0 次调用")
	}
}

// ★ 核心：今天已经成功过 ⇒ 不得补跑（否则每次唤醒都多打一遍上游）。
//
// 传 at(14,0) 而不是 time.Now()：补跑的触发条件里含「已过首个时刻 + 保护期」，
// 用真实时钟会让这个用例在 09:02 之前跑时永远走不到判定点（测试随机器时间
// 变红变绿，是最难查的一类假测试）。
func TestCatchUpCheckinSkipsWhenAlreadySucceeded(t *testing.T) {
	up := &catchupUpstream{statuses: []provider.CheckinStatus{provider.CheckinClaimed}}
	s, p := catchupScheduler(up)
	p.RecordCheckin("u1", true, "已签到") // 持久化「今天成功过」

	s.catchUpCheckin(at(14, 0))
	time.Sleep(300 * time.Millisecond)

	if n := up.calls.Load(); n != 0 {
		t.Fatalf("今天已成功签到却补跑了 %d 次", n)
	}
}

// 今天试过但失败（OK=false）⇒ 交给窗口重试与限流重试，补跑不插手。
func TestCatchUpCheckinSkipsWhenTodayFailed(t *testing.T) {
	up := &catchupUpstream{statuses: []provider.CheckinStatus{provider.CheckinClaimed}}
	s, p := catchupScheduler(up)
	p.RecordCheckin("u1", false, "无可用签到活动")

	s.catchUpCheckin(at(14, 0))
	time.Sleep(300 * time.Millisecond)

	if n := up.calls.Load(); n != 0 {
		t.Fatalf("今日已尝试过（失败）不应补跑，实际 %d 次", n)
	}
}

// 还没到首个配置时刻 ⇒ 不算漏，交给正常调度。
func TestCatchUpCheckinSkipsBeforeFirstSlot(t *testing.T) {
	up := &catchupUpstream{statuses: []provider.CheckinStatus{provider.CheckinClaimed}}
	s, _ := catchupScheduler(up)

	s.catchUpCheckin(at(8, 0))
	time.Sleep(200 * time.Millisecond)

	if n := up.calls.Load(); n != 0 {
		t.Fatalf("未到 09:00 就补跑 %d 次", n)
	}
}

// 保护期：刚好卡在 09:00 时可能撞上整点触发，故 09:00~09:02 不补。
func TestCatchUpCheckinGraceAfterSlot(t *testing.T) {
	up := &catchupUpstream{statuses: []provider.CheckinStatus{provider.CheckinClaimed}}
	s, _ := catchupScheduler(up)

	s.catchUpCheckin(at(9, 1)) // 保护期内
	time.Sleep(200 * time.Millisecond)

	if n := up.calls.Load(); n != 0 {
		t.Fatalf("保护期内不应补跑，实际 %d 次", n)
	}
}

// 没有签到任务的渠道（CheckinMinutes 为空）永不补跑。
func TestCatchUpCheckinSkipsWithoutCheckinTask(t *testing.T) {
	up := &catchupUpstream{statuses: []provider.CheckinStatus{provider.CheckinClaimed}}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	s := New(Config{Pool: p, Upstream: up, Name: "qwenwork", CheckinMinutes: []int{}})

	s.catchUpCheckin(at(14, 0))
	time.Sleep(200 * time.Millisecond)

	if n := up.calls.Load(); n != 0 {
		t.Fatalf("无签到任务的渠道不应补跑，实际 %d 次", n)
	}
}

// ActivitiesOnly 渠道（WorkBuddy 国际版）不落盘签到状态，
// 按状态判定永远「没做过」⇒ 必须显式排除，否则会无限补跑。
func TestCatchUpCheckinSkipsActivitiesOnly(t *testing.T) {
	up := &catchupUpstream{statuses: []provider.CheckinStatus{provider.CheckinClaimed}}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	s := New(Config{
		Pool: p, Upstream: up, Name: "workbuddyai",
		CheckinMinutes: []int{9 * 60}, ActivitiesOnly: true,
	})

	s.catchUpCheckin(at(14, 0))
	time.Sleep(200 * time.Millisecond)

	if n := up.calls.Load(); n != 0 {
		t.Fatalf("ActivitiesOnly 渠道不应补跑，实际 %d 次", n)
	}
}

// ===== 成长自动化补跑 =====

// ★ 核心：旅行开关开着、今天还没自动跑过、已过首个时刻 ⇒ 补跑一次。
func TestCatchUpGrowthRunsTravel(t *testing.T) {
	stub := &growthStub{}
	p := pool.New("")
	p.Add(&auth.Auth{
		Kind: "workbuddy", UID: "u1", RefreshToken: "rt", AccessToken: "at",
		ExpiresAt: time.Now().Add(24 * time.Hour).Unix(),
	})
	s := New(Config{
		Pool: p, Upstream: stub, Name: "workbuddy",
		CheckinMinutes: []int{9 * 60}, GrowthTravel: true, GrowthTravelLocation: 2,
	})

	s.catchUpGrowth(at(14, 0))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if stub.travelCalls > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if stub.travelCalls != 1 {
		t.Fatalf("旅行补跑 travelCalls=%d want 1", stub.travelCalls)
	}
	if stub.lastLocation != 2 {
		t.Fatalf("目的地 lastLocation=%d want 2", stub.lastLocation)
	}
}

// 开关全关 ⇒ 补跑也不得发请求（「默认关闭」承诺不能被兜底逻辑绕过）。
func TestCatchUpGrowthRespectsDisabled(t *testing.T) {
	stub := &growthStub{}
	p := pool.New("")
	p.Add(&auth.Auth{
		Kind: "workbuddy", UID: "u1", RefreshToken: "rt", AccessToken: "at",
		ExpiresAt: time.Now().Add(24 * time.Hour).Unix(),
	})
	s := New(Config{Pool: p, Upstream: stub, Name: "workbuddy", CheckinMinutes: []int{9 * 60}})

	s.catchUpGrowth(at(14, 0))
	time.Sleep(200 * time.Millisecond)

	if stub.travelCalls != 0 || stub.redeemCalls != 0 {
		t.Fatalf("开关关闭却补跑了成长：redeem=%d travel=%d", stub.redeemCalls, stub.travelCalls)
	}
}

// 未到首个配置时刻 ⇒ 成长自动化也不补跑。
func TestCatchUpGrowthSkipsBeforeFirstSlot(t *testing.T) {
	stub := &growthStub{}
	p := pool.New("")
	p.Add(&auth.Auth{
		Kind: "workbuddy", UID: "u1", RefreshToken: "rt", AccessToken: "at",
		ExpiresAt: time.Now().Add(24 * time.Hour).Unix(),
	})
	s := New(Config{
		Pool: p, Upstream: stub, Name: "workbuddy",
		CheckinMinutes: []int{9 * 60}, GrowthTravel: true,
	})

	s.catchUpGrowth(at(8, 0))
	time.Sleep(200 * time.Millisecond)

	if stub.travelCalls != 0 {
		t.Fatalf("未到时刻却补跑了旅行 %d 次", stub.travelCalls)
	}
}

// ===== catchUpToday 的每日幂等 =====

// ★ 核心：Run 每次唤醒都会调 catchUpToday，必须保证每天只判定一次。
// 少了这条，一个整天运行的进程会每小时补跑一次签到。
func TestCatchUpTodayIsIdempotent(t *testing.T) {
	up := &catchupUpstream{statuses: []provider.CheckinStatus{provider.CheckinClaimed}}
	s, _ := catchupScheduler(up)
	now := at(14, 0)

	s.catchUpToday(now)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if up.calls.Load() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	first := up.calls.Load()
	if first == 0 {
		t.Fatal("首次 catchUpToday 应补跑一次签到，实际 0 次")
	}

	// 同一天再调三次：不得再发任何请求。
	for i := 0; i < 3; i++ {
		s.catchUpToday(now)
	}
	time.Sleep(300 * time.Millisecond)

	if n := up.calls.Load(); n != first {
		t.Fatalf("同日重复 catchUpToday 又补跑了：%d → %d", first, n)
	}
}

// ★ 核心：补跑**失败**后，同一天不得再补。
//
// 这条是 catchupDoneDay 唯一无法被持久化状态替代的场景：补跑失败不写
// LastCheckinOK=true，若只靠状态判定，进程每小时唤醒都会再试一次 ——
// 上游明明没活动，却整天被反复打。交给窗口重试与按账号的限流重试去收敛。
func TestCatchUpTodayDoesNotRepeatAfterFailure(t *testing.T) {
	up := &catchupUpstream{statuses: []provider.CheckinStatus{provider.CheckinNoCampaign}}
	s, _ := catchupScheduler(up)
	now := at(14, 0)
	day := now.Format("2006-01-02")

	s.catchUpToday(now)
	// 直接断言标记被置位：这是 catchupDoneDay 存在的唯一理由，
	// 只靠「网络调用次数没涨」来测，会被持久化状态顺带兜住而测不出变异。
	if got := s.catchupDoneDay; got != day {
		t.Fatalf("catchupDoneDay=%q want %q", got, day)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if up.calls.Load() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	first := up.calls.Load()
	if first == 0 {
		t.Fatal("首次 catchUpToday 应补跑一次签到，实际 0 次")
	}

	for i := 0; i < 3; i++ {
		s.catchUpToday(now)
	}
	time.Sleep(300 * time.Millisecond)

	if n := up.calls.Load(); n != first {
		t.Fatalf("补跑失败后同日又重试了：%d → %d（catchupDoneDay 未生效）", first, n)
	}
}
