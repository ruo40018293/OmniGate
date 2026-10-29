package app

import (
	"log"
	"net/http"
	"time"

	"omnigate/internal/scheduler"
)

// ---------------------------------------------------------------------------
// 账号批量作业（刷新积分 / 全部签到）：立刻受理 + 进度可查
// ---------------------------------------------------------------------------
//
// 为什么必须异步：一次批量作业 = 「渠道 × 账号」次上游往返。生产日志 17 天实测：
// refresh_all 286 次里 229 次超过 1s、最慢 122.99s；checkin_all 最慢 14.95s。
// 同步 HTTP 会让浏览器在这段时间里既拿不到结果也没有任何进度 —— 用户分不清
// 「在跑」和「卡死」，只能反复点。而面板的硬要求是「等待可以，但必须看得见」
// （R56 可感知、R73 轮询收敛）。
//
// 形态：POST 立刻回 {started}，作业在后台推进计数，GET *_status 回只读快照。

// bulkKind 批量作业类型（同时是 *_status 载荷里的 kind 字段）。
type bulkKind string

const (
	bulkRefresh bulkKind = "refresh"
	bulkCheckin bulkKind = "checkin"
)

// bulkTarget 一个待处理账号。
//
// 计数与执行必须共用同一份 targets —— 各枚举一遍会让「进度总数」和
// 「实际处理数」按两套判据算，进度条先跳后停。
type bulkTarget struct {
	rt       *Runtime
	uid      string
	disabled bool
}

// bulkProgress 逐账号进度回调；零值（同步路径）全部方法是 no-op。
type bulkProgress struct {
	onStart func(platform, uid string)
	onDone  func(ok bool)
}

func (p bulkProgress) start(platform, uid string) {
	if p.onStart != nil {
		p.onStart(platform, uid)
	}
}

func (p bulkProgress) done(ok bool) {
	if p.onDone != nil {
		p.onDone(ok)
	}
}

// bulkRun 一次作业的执行状态（读写都由 App.refreshMu 保护）。
type bulkRun struct {
	kind       bulkKind
	running    bool
	startedAt  time.Time
	finishedAt time.Time
	total      int
	done       int
	ok         int
	failed     int
	current    string // 正在处理的「渠道/账号」，给面板做 title
	refresh    *RefreshSummary
	checkin    []scheduler.CheckinResult
}

// bulkView 面板轮询用的只读快照（几十字节级，可以 500ms 一次）。
type bulkView struct {
	Kind       string                    `json:"kind,omitempty"`
	Running    bool                      `json:"running"`
	StartedAt  string                    `json:"started_at,omitempty"`
	FinishedAt string                    `json:"finished_at,omitempty"`
	Total      int                       `json:"total"`
	Done       int                       `json:"done"`
	OK         int                       `json:"ok"`
	Failed     int                       `json:"failed"`
	Current    string                    `json:"current,omitempty"`
	Refresh    *RefreshSummary           `json:"refresh,omitempty"`
	Checkin    []scheduler.CheckinResult `json:"checkin,omitempty"`
}

// refreshTargets 刷新积分要处理的账号。
//
// 过滤判据：别名渠道与主渠道共用同一 Pool，计入会重复刷新同一批账号；
// 无 Pool / 无 Upstream 的渠道无处刷新。
func (a *App) refreshTargets() []bulkTarget {
	out := make([]bulkTarget, 0, len(a.runtimes))
	for _, rt := range a.runtimes {
		if rt == nil || rt.Pool == nil || rt.Upstream == nil || rt.Alias {
			continue
		}
		for _, st := range rt.Pool.List() {
			out = append(out, bulkTarget{rt: rt, uid: st.UID, disabled: st.Disabled})
		}
	}
	return out
}

// checkinTargets 批量签到要处理的账号（无签到活动的渠道整条跳过）。
func (a *App) checkinTargets() []bulkTarget {
	out := make([]bulkTarget, 0, len(a.runtimes))
	for _, rt := range a.runtimes {
		// 别名渠道复用同一 Pool 且无独立 Scheduler：计入会重复签到同一批账号。
		if rt == nil || rt.Pool == nil || rt.Scheduler == nil || rt.Alias {
			continue
		}
		if noExplicitCheckin(rt.Kind) { // 无签到活动渠道，跳过
			continue
		}
		for _, st := range rt.Pool.List() {
			out = append(out, bulkTarget{rt: rt, uid: st.UID, disabled: st.Disabled})
		}
	}
	return out
}

// StartBulk 启动一次批量作业，返回是否真的启动。
//
// 已在跑时返回 false 而不是排队：两个批量作业叠着跑会互相挤占上游配额，
// 「点了没反应」和「点了跑两遍」一样糟。
func (a *App) StartBulk(kind bulkKind) bool {
	var targets []bulkTarget
	switch kind {
	case bulkRefresh:
		targets = a.refreshTargets()
	case bulkCheckin:
		targets = a.checkinTargets()
	default:
		return false
	}

	a.refreshMu.Lock()
	if a.bulk != nil && a.bulk.running {
		a.refreshMu.Unlock()
		return false
	}
	st := &bulkRun{kind: kind, running: true, startedAt: time.Now(), total: len(targets)}
	a.bulk = st
	a.refreshMu.Unlock()

	log.Printf("bulk job start kind=%s accounts=%d", kind, len(targets))
	a.safeGo(func() { a.runBulk(st, targets) })
	return true
}

// runBulk 执行作业并推进进度。
func (a *App) runBulk(st *bulkRun, targets []bulkTarget) {
	// 无论正常结束还是 panic 都要落「已完成」——否则面板永远转圈，
	// 而且后续请求都被告知「已有任务进行中」，等于功能死锁。
	defer a.finishBulk(st)

	p := bulkProgress{
		onStart: func(platform, uid string) {
			a.refreshMu.Lock()
			st.current = platform + "/" + uid
			a.refreshMu.Unlock()
		},
		onDone: func(ok bool) {
			a.refreshMu.Lock()
			st.done++
			if ok {
				st.ok++
			} else {
				st.failed++
			}
			a.refreshMu.Unlock()
		},
	}

	switch st.kind {
	case bulkRefresh:
		sum := a.refreshAllWithProgress(p, targets)
		a.refreshMu.Lock()
		st.refresh = &sum
		a.refreshMu.Unlock()
	case bulkCheckin:
		results := a.checkinAllWithProgress(p, targets)
		a.refreshMu.Lock()
		st.checkin = results
		a.refreshMu.Unlock()
	}
}

// finishBulk 收尾：清运行标志、留完成时间与耗时日志。
func (a *App) finishBulk(st *bulkRun) {
	a.refreshMu.Lock()
	st.running = false
	st.finishedAt = time.Now()
	st.current = ""
	kind, done, total, failed := st.kind, st.done, st.total, st.failed
	elapsed := st.finishedAt.Sub(st.startedAt)
	a.refreshMu.Unlock()
	log.Printf("bulk job finish kind=%s done=%d/%d failed=%d elapsed=%s",
		kind, done, total, failed, elapsed.Round(time.Millisecond))
}

// BulkView 回只读快照（尚未跑过任何作业时返回零值，Running=false）。
func (a *App) BulkView() bulkView {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	if a.bulk == nil {
		return bulkView{}
	}
	st := a.bulk
	v := bulkView{
		Kind:    string(st.kind),
		Running: st.running,
		Total:   st.total,
		Done:    st.done,
		OK:      st.ok,
		Failed:  st.failed,
		Current: st.current,
		Checkin: append([]scheduler.CheckinResult(nil), st.checkin...),
	}
	if !st.startedAt.IsZero() {
		v.StartedAt = st.startedAt.Format(time.RFC3339)
	}
	if !st.finishedAt.IsZero() {
		v.FinishedAt = st.finishedAt.Format(time.RFC3339)
	}
	if st.refresh != nil {
		sum := *st.refresh
		v.Refresh = &sum
	}
	return v
}

// acceptBulk 「立刻受理」的公共应答：只回是否启动成功，结果去 *_status 取。
func (a *App) acceptBulk(w http.ResponseWriter, kind bulkKind) {
	started := a.StartBulk(kind)
	writeJSON(w, http.StatusOK, map[string]any{"started": started, "busy": !started})
}
