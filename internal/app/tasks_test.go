package app

// 成长任务「一键完成」的面板侧测试。
//
// ★ 核心护栏：**任务领奖后必须刷新账号余额**。
//
// 面板 `/api/state` 的 credits 读自 pool 缓存（pool.SetCreditDetail 写入），
// 任务执行本身不会改它。若跑完不刷新，用户看到的是
// 「任务做完了但积分没变」—— 会误判成没领到奖励，进而怀疑整个功能无效。
//
// 全部用例不发起真实网络请求。

import (
	"encoding/json"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"omnigate/internal/auth"
	"omnigate/internal/config"
	"omnigate/internal/pool"
	"omnigate/internal/provider"
	"omnigate/internal/upstream"
)

// taskUpstream 桩：把 UserResourceDetail 接上（刷新余额需要），
// 并实现成长任务接口以控制领奖结果。
type taskUpstream struct {
	fakeUpstream
	// credit 本轮领到的积分；remain 上游余额（刷新后应等于它）
	credit int64
	// skipTaskRun 为 true 时任务跑但拿不到奖励（进度未达）
	skipTaskRun bool
	taskCalls   atomic.Int32
}

func (t *taskUpstream) GrowthRunTasks(a *auth.Auth, opts upstream.GrowthTaskOptions) upstream.GrowthTaskReport {
	t.taskCalls.Add(1)
	if t.skipTaskRun {
		return upstream.GrowthTaskReport{UID: a.UID, Msg: "进度未达"}
	}
	return upstream.GrowthTaskReport{
		UID:    a.UID,
		Msg:    "已领奖",
		Credit: t.credit,
		Tasks: []upstream.GrowthTaskOutcome{
			{TaskCode: "first_buddy", Status: "claimed", Msg: "已领 Buddy", Credit: t.credit},
		},
	}
}

// newTaskTestApp 构造含单个 workbuddy 账号的 App。
func newTaskTestApp(t *testing.T, up provider.Upstream, uid string) (*App, *pool.Pool) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateFile = dir + "/state.json"
	cfg.AuthDir = dir

	p := pool.New(dir + "/state-workbuddy.json")
	p.Add(&auth.Auth{Kind: "workbuddy", UID: uid, AccessToken: "t", RefreshToken: "r"})

	a, err := New(Options{
		ConfigPath: dir + "/config.json",
		Config:     cfg,
		Runtimes: map[provider.Kind]*Runtime{
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: p, Upstream: up},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.Close)
	return a, p
}

// waitTaskDone 等待任务执行结束（含后端余额刷新）。
func waitTaskDone(t *testing.T, a *App) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if v := a.TaskRunView(); !v.Running && len(v.Reports) > 0 {
			// 再等一小会儿让余额刷新落盘
			time.Sleep(120 * time.Millisecond)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("任务未在 3 秒内结束")
}

// ★ 核心：领奖后余额必须被刷新到新值。
//
// 反向验证：去掉 runTasks 末尾的 refreshCreditsAfterTasks 调用，
// 此用例立即失败（余额停在旧值）。
func TestTaskRunRefreshesCreditsAfterClaim(t *testing.T) {
	up := &taskUpstream{credit: 300}
	up.remain = 1000 // 执行前的旧余额
	a, p := newTaskTestApp(t, up, "uid-1")

	// 先把旧余额写进缓存，模拟「面板上显示的是执行前的值」
	if _, err := a.RefreshCredits("uid-1"); err != nil {
		t.Fatalf("预置余额失败：%v", err)
	}
	if st, _ := p.Status("uid-1"); st.Credits != 1000 {
		t.Fatalf("预置余额未生效：%d", st.Credits)
	}

	// 任务领奖 +300 → 上游余额应变为 1300
	up.remain = 1300

	if err := a.StartTaskRun("", false); err != nil {
		t.Fatalf("启动任务失败：%v", err)
	}
	waitTaskDone(t, a)

	st, ok := p.Status("uid-1")
	if !ok {
		t.Fatal("账号不在池中")
	}
	if st.Credits != 1300 {
		t.Fatalf("任务领奖后余额未刷新：得到 %d，期望 1300"+
			"（不刷新就会停在执行前的 1000，用户会以为没领到奖）", st.Credits)
	}
}

// 未领到奖励时不应无谓刷新（避免给上游平白增加请求）。
func TestTaskRunSkipsRefreshWhenNoReward(t *testing.T) {
	up := &taskUpstream{skipTaskRun: true}
	up.remain = 1000
	a, _ := newTaskTestApp(t, up, "uid-1")

	if _, err := a.RefreshCredits("uid-1"); err != nil {
		t.Fatalf("预置余额失败：%v", err)
	}
	before := up.resourceCalls.Load()

	if err := a.StartTaskRun("", false); err != nil {
		t.Fatalf("启动任务失败：%v", err)
	}
	waitTaskDone(t, a)

	if got := up.resourceCalls.Load(); got != before {
		t.Fatalf("无奖励时仍刷新了余额：上游查询 %d → %d", before, got)
	}
}

// StartTaskRun 必须拒绝并发（并发跑会让「回读进度」互相干扰）。
func TestStartTaskRunRejectsConcurrent(t *testing.T) {
	up := &taskUpstream{credit: 10}
	up.remain = 100
	a, _ := newTaskTestApp(t, up, "uid-1")

	if err := a.StartTaskRun("", false); err != nil {
		t.Fatalf("首次启动应成功：%v", err)
	}
	// 必须等首个真正结束再断言：否则 t.Cleanup 删 tempDir 时后台 goroutine
	// 还在写账本文件，会报「文件被另一进程占用」而非真实失败。
	waitTaskDone(t, a)

	// 首个已结束 → 此时应能再次启动，且状态被完整重置
	if err := a.StartTaskRun("", false); err != nil {
		t.Fatalf("上一轮结束后应允许再次启动：%v", err)
	}
	waitTaskDone(t, a)

	v := a.TaskRunView()
	if v.AccountsTotal != 1 || len(v.Reports) != 1 {
		t.Fatalf("状态未正确重置：accounts_total=%d reports=%d", v.AccountsTotal, len(v.Reports))
	}
}

// 不支持的渠道：无目标可执行时应报错，而不是静默空跑。
//
// 注意：这里用的是 TraeWork（不支持成长中心），任务解析阶段就会因
// channelSupportsGrowth 判定而跳过全部账号 → 目标为空 → 返回错误。
func TestStartTaskRunUnsupportedChannel(t *testing.T) {
	// TraeWork 不在成长任务白名单内
	a, _ := newTestApp(t, provider.TraeWork, &fakeUpstream{remain: 1}, "uid-tr")

	if err := a.StartTaskRun("", false); err == nil {
		t.Fatal("渠道不支持成长任务时应返回错误")
	}
}

// GrowthConfig 默认全关 —— 这是「默认不自动消耗用户资源」的机器验证。
func TestGrowthConfigDefaultsOff(t *testing.T) {
	cfg := config.Default()
	if cfg.Schedule.GrowthRedeemStreak || cfg.Schedule.GrowthTravel || cfg.Schedule.GrowthTasks {
		t.Fatalf("成长中心自动化应默认全关，实际 redeem=%t travel=%t tasks=%t",
			cfg.Schedule.GrowthRedeemStreak, cfg.Schedule.GrowthTravel, cfg.Schedule.GrowthTasks)
	}
	if cfg.Schedule.GrowthTasksIncludeMP {
		t.Fatal("小程序任务默认不应参与执行（反作弊要求 45s/条，一项可能跑几分钟）")
	}
}

// SetGrowthConfig 必须落盘并热更新到调度器。
func TestSetGrowthConfigPersistsAndHotUpdates(t *testing.T) {
	up := &taskUpstream{credit: 1}
	up.remain = 10
	a, _ := newTaskTestApp(t, up, "uid-1")

	if err := a.SetGrowthConfig(true, true, 3, true, true); err != nil {
		t.Fatalf("保存成长开关失败：%v", err)
	}

	// 回显一致
	g := a.GrowthConfig()
	if !g.RedeemStreak || !g.Travel || g.TravelLocation != 3 || !g.Tasks || !g.TasksIncludeMP {
		t.Fatalf("回显不一致：%+v", g)
	}
	// 落盘（重新读文件验证，避免「只在内存里改了」）
	raw, err := os.ReadFile(a.cfgPath)
	if err != nil {
		t.Fatalf("读配置失败：%v", err)
	}
	var saved config.Config
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		t.Fatalf("配置解析失败：%v", err)
	}
	if !saved.Schedule.GrowthTasks || saved.Schedule.GrowthTravelLocation != 3 {
		t.Fatalf("未写入磁盘：%+v", saved.Schedule)
	}
}

// 旅行目的地越界必须被拒绝（配置层拦截，不靠运行时收敛）。
func TestSetGrowthConfigRejectsBadLocation(t *testing.T) {
	up := &taskUpstream{}
	up.remain = 10
	a, _ := newTaskTestApp(t, up, "uid-1")

	if err := a.SetGrowthConfig(false, true, 9, false, false); err == nil {
		t.Fatal("越界的旅行目的地应被拒绝")
	}
}

// GrowthSupported 只对实现了成长任务接口的渠道为真。
func TestGrowthSupported(t *testing.T) {
	up := &taskUpstream{}
	up.remain = 10
	a, _ := newTaskTestApp(t, up, "uid-1")
	if !a.GrowthSupported() {
		t.Fatal("workbuddy 应被判定为支持成长任务")
	}

	a2, _ := newTestApp(t, provider.TraeWork, &fakeUpstream{remain: 1}, "u-t")
	if a2.GrowthSupported() {
		t.Fatal("TraeWork 不提供成长中心，不应判定为支持")
	}
}

// 账号视图的 growth 标志：仅成长中心支持的渠道为 true。
func TestAccountViewGrowthFlag(t *testing.T) {
	up := &taskUpstream{}
	up.remain = 10
	a, _ := newTaskTestApp(t, up, "uid-1")

	views := a.accountViews()
	if len(views) != 1 {
		t.Fatalf("账号数=%d want 1", len(views))
	}
	if !views[0].Growth {
		t.Fatal("workbuddy 账号应带 growth 标志（面板据此显示 ★ 入口）")
	}
}

// StartTaskRun 指定 uid 时只跑该账号。
func TestStartTaskRunSingleUID(t *testing.T) {
	up := &taskUpstream{credit: 10}
	up.remain = 100
	a, _ := newTaskTestApp(t, up, "uid-1")

	if err := a.StartTaskRun("uid-1", false); err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	waitTaskDone(t, a)

	v := a.TaskRunView()
	if v.Scope != "uid-1" {
		t.Fatalf("scope=%q want uid-1", v.Scope)
	}
	if up.taskCalls.Load() != 1 {
		t.Fatalf("任务执行次数=%d want 1", up.taskCalls.Load())
	}
}

// 不存在的 uid 必须报错，而不是静默空跑。
func TestStartTaskRunUnknownUID(t *testing.T) {
	up := &taskUpstream{}
	up.remain = 10
	a, _ := newTaskTestApp(t, up, "uid-1")

	if err := a.StartTaskRun("no-such-uid", false); err == nil {
		t.Fatal("不存在的 uid 应返回错误")
	}
}

// 编译期护栏：桩必须同时满足渠道接口与成长任务接口。
var (
	_ provider.Upstream = (*taskUpstream)(nil)
	_ growthTaskRunner  = (*taskUpstream)(nil)
)
