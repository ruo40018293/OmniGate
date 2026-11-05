package app

import (
	"path/filepath"
	"testing"

	"omnigate/internal/auth"
	"omnigate/internal/config"
	"omnigate/internal/pool"
	"omnigate/internal/provider"
	"omnigate/internal/scheduler"
)

// newCheckinPushApp 构造「一个跟随全局时刻的渠道 + 一个固定窗口渠道」的最小 App。
func newCheckinPushApp(t *testing.T) (*App, *scheduler.Scheduler, *scheduler.Scheduler) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.AuthDir = dir

	wbPool := pool.New(filepath.Join(dir, "state-workbuddy.json"))
	wbPool.Add(&auth.Auth{Kind: "workbuddy", UID: "wb-1", AccessToken: "t", RefreshToken: "r"})
	wbSch := scheduler.New(scheduler.Config{
		Pool: wbPool, Upstream: &fakeUpstream{remain: 10}, Name: "workbuddy",
		CheckinMinutes: []int{9 * 60, 21 * 60}, FollowGlobalCheckin: true,
	})

	qcPool := pool.New(filepath.Join(dir, "state-qodercn.json"))
	qcPool.Add(&auth.Auth{Kind: "qodercn", UID: "qc-1", AccessToken: "t", RefreshToken: "r"})
	// Qoder 双区固定在 10:00 开放，绝不能跟随面板全局时刻。
	qcSch := scheduler.New(scheduler.Config{
		Pool: qcPool, Upstream: &fakeUpstream{remain: 10}, Name: "qodercn",
		CheckinMinutes: []int{10 * 60},
	})

	a, err := New(Options{
		ConfigPath: filepath.Join(dir, "config.json"),
		Config:     cfg,
		Runtimes: map[provider.Kind]*Runtime{
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: wbPool, Upstream: &fakeUpstream{remain: 10}, Scheduler: wbSch},
			provider.QoderCN:   {Kind: provider.QoderCN, Pool: qcPool, Upstream: &fakeUpstream{remain: 10}, Scheduler: qcSch},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.Close)
	return a, wbSch, qcSch
}

// R94：面板改签到时间，只推给声明跟随全局时刻的渠道。
//
// 反例（改前）：无条件推给所有渠道，于是 Qoder 双区那条「10:00 才开放」的
// 固定窗口被用户面板上的时间覆盖，当天直接漏领；无签到渠道则开始每天空跑。
func TestSetCheckinTimesOnlyPushesFollowers(t *testing.T) {
	a, wbSch, qcSch := newCheckinPushApp(t)

	if err := a.SetCheckinTimes([]string{"08:15", "23:45"}); err != nil {
		t.Fatalf("SetCheckinTimes: %v", err)
	}

	got := wbSch.CheckinMinutes()
	if len(got) != 2 || got[0] != 8*60+15 || got[1] != 23*60+45 {
		t.Fatalf("跟随全局的渠道必须收到新时刻 [495 1425]，实际 %v", got)
	}

	forced := qcSch.CheckinMinutes()
	if len(forced) != 1 || forced[0] != 10*60 {
		t.Fatalf("固定窗口渠道的时刻不得被覆盖，应仍为 [600]，实际 %v", forced)
	}
}

// 全局时刻仍要落到配置里（否则重启后停在自己都不知道的值上）。
func TestSetCheckinTimesPersistsToConfig(t *testing.T) {
	a, _, _ := newCheckinPushApp(t)

	if err := a.SetCheckinTimes([]string{"07:05"}); err != nil {
		t.Fatalf("SetCheckinTimes: %v", err)
	}
	if len(a.cfg.Schedule.CheckinTimes) != 1 || a.cfg.Schedule.CheckinTimes[0] != "07:05" {
		t.Fatalf("配置应保存 07:05，实际 %v", a.cfg.Schedule.CheckinTimes)
	}
	if len(a.cfg.Schedule.CheckinHours) != 0 {
		t.Fatalf("CheckinHours 必须清空以让 CheckinTimes 生效，实际 %v", a.cfg.Schedule.CheckinHours)
	}
}
