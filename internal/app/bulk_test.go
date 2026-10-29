package app

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"omnigate/internal/auth"
	"omnigate/internal/config"
	"omnigate/internal/pool"
	"omnigate/internal/provider"
)

// gatedUpstream 在 UserResourceDetail 入口挂一道闸：测试用它把批量作业
// 卡在「进行中」，从而真正验证 running/done 的中间态（只测终态等于没测异步）。
type gatedUpstream struct {
	fakeUpstream
	gate chan struct{}
}

func (g *gatedUpstream) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	<-g.gate
	return g.fakeUpstream.UserResourceDetail(a)
}

// newBulkApp 构造带 N 个账号的最小 App；up 决定上游行为。
func newBulkApp(t *testing.T, up provider.Upstream, uids ...string) *App {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.AuthDir = dir
	cfg.Listen = config.Listen{Host: "127.0.0.1", Port: 7863}

	p := pool.New(filepath.Join(dir, "state-workbuddy.json"))
	for _, uid := range uids {
		p.Add(&auth.Auth{Kind: "workbuddy", UID: uid, AccessToken: "t", RefreshToken: "r"})
	}
	a, err := New(Options{
		ConfigPath: filepath.Join(dir, "config.json"),
		Config:     cfg,
		Runtimes: map[provider.Kind]*Runtime{
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: p, Upstream: up},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.Close)
	return a
}

// waitBulkFinished 等作业结束；作业要走真实上游往返，给足 3 秒。
func waitBulkFinished(t *testing.T, a *App) bulkView {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if v := a.BulkView(); !v.Running {
			return v
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("批量作业 3s 内未结束：%+v", a.BulkView())
	return bulkView{}
}

// TestBulkRefreshRunningStateThenSuccess 刷新积分：受理即回、进行中能看到
// 「处理了几个 / 共几个」、结束后有完整汇总。
func TestBulkRefreshRunningStateThenSuccess(t *testing.T) {
	up := &gatedUpstream{gate: make(chan struct{})}
	a := newBulkApp(t, up, "wb-a", "wb-b")

	if !a.StartBulk(bulkRefresh) {
		t.Fatal("空闲时批量刷新应能启动")
	}

	// 上游闸门还关着：必须 running、总数已定、done 还没走完。
	v := a.BulkView()
	if !v.Running {
		t.Fatalf("受理后应立即 running，实际 %+v", v)
	}
	if v.Kind != string(bulkRefresh) {
		t.Fatalf("kind 应为 refresh，实际 %q", v.Kind)
	}
	if v.Total != 2 {
		t.Fatalf("总数应为 2（两个账号），实际 %d", v.Total)
	}
	if v.Done != 0 {
		t.Fatalf("上游还没返回就不该有 done：done=%d", v.Done)
	}

	close(up.gate) // 放行两个账号
	v = waitBulkFinished(t, a)
	if v.OK != 2 || v.Failed != 0 {
		t.Fatalf("应 2 成功 0 失败，实际 ok=%d failed=%d", v.OK, v.Failed)
	}
	if v.Done != v.Total {
		t.Fatalf("结束时 done 必须等于 total：done=%d total=%d", v.Done, v.Total)
	}
	if v.Current != "" {
		t.Fatalf("结束后不该还留 current：%q", v.Current)
	}
	if v.Refresh == nil || v.Refresh.Total != 2 || v.Refresh.OK != 2 {
		t.Fatalf("汇总应带上 RefreshSummary：%+v", v.Refresh)
	}
}

// TestBulkRefreshCountsFailures 单个账号失败不中断整批，且计入 failed。
func TestBulkRefreshCountsFailures(t *testing.T) {
	up := &fakeUpstream{remain: 5}
	up.failResource.Store(true)
	a := newBulkApp(t, up, "wb-a", "wb-b")
	if !a.StartBulk(bulkRefresh) {
		t.Fatal("批量刷新应能启动")
	}
	v := waitBulkFinished(t, a)
	if v.OK != 0 || v.Failed != 2 {
		t.Fatalf("应 0 成功 2 失败，实际 ok=%d failed=%d", v.OK, v.Failed)
	}
	if v.Refresh == nil || len(v.Refresh.Platforms) != 1 {
		t.Fatalf("失败也要出现在汇总里：%+v", v.Refresh)
	}
	for _, ps := range v.Refresh.Platforms {
		for _, acc := range ps.Accounts {
			if acc.OK || acc.Msg == "" {
				t.Fatalf("失败账号必须带原因：%+v", acc)
			}
		}
	}
}

// TestBulkRejectsConcurrentStart 同一时刻只允许一个批量作业：
// 叠着跑会互相挤占上游配额，也会让「进度」变成两批账号的混算。
func TestBulkRejectsConcurrentStart(t *testing.T) {
	up := &gatedUpstream{gate: make(chan struct{})}
	a := newBulkApp(t, up, "wb-a")
	if !a.StartBulk(bulkRefresh) {
		t.Fatal("第一个作业应能启动")
	}

	if a.StartBulk(bulkRefresh) {
		t.Fatal("作业进行中时第二次启动应被拒绝")
	}
	// 换成签到也一样被拒（作业槽是共享的）
	if a.StartBulk(bulkCheckin) {
		t.Fatal("作业进行中时不应允许另一种批量作业插队")
	}
	if v := a.BulkView(); v.Kind != string(bulkRefresh) {
		t.Fatalf("被拒的启动不得覆盖当前作业：kind=%q", v.Kind)
	}

	// 作业 goroutine 还在往 t.TempDir() 里写文件：必须等它真正结束再退出，
	// 否则 TempDir 清理会撞上「目录非空」（Windows 上必现）。
	close(up.gate)
	waitBulkFinished(t, a)
}

// TestBulkEndpointsAcceptThenPoll HTTP 形态：POST 只回 started，结果去 *_status 取。
func TestBulkEndpointsAcceptThenPoll(t *testing.T) {
	a := newBulkApp(t, &fakeUpstream{remain: 7}, "wb-a")
	mux := panelMux(a)

	w := doReq(mux, "POST", "/api/account/refresh_all", "", nil)
	if w.Code != 200 {
		t.Fatalf("受理应 200，实际 %d", w.Code)
	}
	var acc struct {
		Started bool `json:"started"`
		Busy    bool `json:"busy"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &acc); err != nil {
		t.Fatalf("解析受理响应: %v（%s）", err, w.Body.String())
	}
	if !acc.Started || acc.Busy {
		t.Fatalf("首次受理应 started=true busy=false，实际 %+v", acc)
	}

	waitBulkFinished(t, a)
	w = doReq(mux, "GET", "/api/account/refresh_all_status", "", nil)
	if w.Code != 200 {
		t.Fatalf("状态端点应 200，实际 %d", w.Code)
	}
	var v bulkView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("解析状态: %v（%s）", err, w.Body.String())
	}
	if v.Running || v.OK != 1 || v.Total != 1 {
		t.Fatalf("状态应 finished ok=1 total=1，实际 %+v", v)
	}
}

// TestBulkCheckinEmptyTargetsStillCloses 没有可签到账号时也必须落「已完成」，
// 否则面板会永远转圈、后续请求全被告知「已有任务进行中」。
func TestBulkCheckinEmptyTargetsStillCloses(t *testing.T) {
	// 该 App 的 Runtime 没有 Scheduler → checkinTargets 为空。
	a := newBulkApp(t, &fakeUpstream{remain: 1}, "wb-a")
	mux := panelMux(a)

	w := doReq(mux, "POST", "/api/account/checkin_all", "", nil)
	if w.Code != 200 {
		t.Fatalf("受理应 200，实际 %d", w.Code)
	}
	v := waitBulkFinished(t, a)
	if v.Kind != string(bulkCheckin) {
		t.Fatalf("kind 应为 checkin，实际 %q", v.Kind)
	}
	if v.Total != 0 || v.Done != 0 {
		t.Fatalf("无目标时应 total=0 done=0，实际 %+v", v)
	}
	// 结束后必须能再次启动
	if !a.StartBulk(bulkCheckin) {
		t.Fatal("上一次作业收尾后应允许再次启动")
	}
}
