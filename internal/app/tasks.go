// tasks.go 成长任务「一键完成」的面板侧：异步执行 + 进度查询 + 开关热更新。
//
// ## 为什么是异步
//
// 一轮任务要发几十个请求（含真实对话、专家召唤链），单账号几十秒、
// 多账号可到数分钟。同步 HTTP 会撞上前端 fetch 与反向代理的超时，
// 用户看到的是「点了没反应」。故采用：
//
//	POST /api/account/tasks        启动，立即返回
//	GET  /api/account/tasks_status 轮询进度与结果
//
// ## 风险提示（面板上也会显示）
//
// 该功能靠**伪造客户端行为事件**点亮任务进度，会真实改变账号状态
// （点亮进度、领取奖励）。事件形状来自第三方逆向成果，本项目未独立实测，
// 因此：
//   - 默认**不**随定时任务自动执行（config.schedule.growth_tasks 默认 false）
//   - 只能由用户在面板上显式点击触发
//   - 首次建议只对一个账号执行，核对结果后再考虑开启自动
package app

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"omnigate/internal/auth"
	"omnigate/internal/config"
	"omnigate/internal/provider"
	"omnigate/internal/upstream"
)

// growthTaskRunner 由支持成长任务的上游客户端实现（当前为 workbuddy 渠道）。
//
// 用接口而非直接断言具体类型：App 不关心是哪个渠道的客户端。
type growthTaskRunner interface {
	GrowthRunTasks(a *auth.Auth, opts upstream.GrowthTaskOptions) upstream.GrowthTaskReport
}

// growthTraveler 由支持「猫猫旅行」的上游客户端实现（当前为 workbuddy 渠道）。
//
// 与 growthTaskRunner 分立两个接口：旅行只有「查状态 → 领取/派出」1~2 次往返，
// 结果是一条短摘要；成长任务要几十个请求、要进度回调。合成一个接口会逼着
// 所有渠道同时实现两套方法。App 只探测当前需要的那个。
type growthTraveler interface {
	GrowthTravelOnce(a *auth.Auth, locationID int) (msg string, err error)
}

// taskRunState 一次任务执行的进度与结果（受 App.taskMu 保护）。
type taskRunState struct {
	running    bool
	startedAt  time.Time
	finishedAt time.Time
	scope      string // "all" 或具体 uid
	includeMP  bool
	accTotal   int
	accDone    int
	current    string
	reports    []upstream.GrowthTaskReport
	errMsg     string
}

// taskRunView 面板轮询用的只读视图。
type taskRunView struct {
	Running       bool                        `json:"running"`
	StartedAt     string                      `json:"started_at,omitempty"`
	FinishedAt    string                      `json:"finished_at,omitempty"`
	Scope         string                      `json:"scope,omitempty"`
	IncludeMP     bool                        `json:"include_mp"`
	AccountsTotal int                         `json:"accounts_total"`
	AccountsDone  int                         `json:"accounts_done"`
	Current       string                      `json:"current,omitempty"`
	Reports       []upstream.GrowthTaskReport `json:"reports,omitempty"`
	Credit        int64                       `json:"credit"`
	Energy        int64                       `json:"energy"`
	Err           string                      `json:"err,omitempty"`
}

// StartTaskRun 启动一次成长任务执行（异步）。
//
// uid 为空 = 对所有支持成长中心的账号执行；includeMP 决定是否包含
// 小程序口径任务（默认不建议：反作弊要求 45s/条的节奏，会很慢）。
//
// 同一时刻只允许一次执行（并发跑会让「回读进度」互相干扰）。
func (a *App) StartTaskRun(uid string, includeMP bool) error {
	targets := a.taskTargets(uid)
	if len(targets) == 0 {
		if strings.TrimSpace(uid) != "" {
			return fmt.Errorf("账号不存在或不支持成长任务：%s", uid)
		}
		return errors.New("没有支持成长任务的账号（成长中心目前仅 WorkBuddy 国内版提供）")
	}

	a.taskMu.Lock()
	if a.taskRun != nil && a.taskRun.running {
		a.taskMu.Unlock()
		return errors.New("已有任务正在执行，请等待本轮结束")
	}
	scope := "all"
	if strings.TrimSpace(uid) != "" {
		scope = uid
	}
	st := &taskRunState{
		running:   true,
		startedAt: time.Now(),
		scope:     scope,
		includeMP: includeMP,
		accTotal:  len(targets),
	}
	a.taskRun = st
	a.taskMu.Unlock()

	log.Printf("task run start scope=%s include_mp=%t accounts=%d", scope, includeMP, len(targets))

	go a.runTasks(st, targets, includeMP)
	return nil
}

// taskTarget 一个待执行账号（渠道运行时 + 账号）。
type taskTarget struct {
	rt *Runtime
	a  *auth.Auth
}

// runTasks 后台执行主体。单个账号失败不中断整批。
func (a *App) runTasks(st *taskRunState, targets []taskTarget, includeMP bool) {
	// 本轮有到账的账号（跑完再刷新它们的积分）。
	var credited []string
	for _, tgt := range targets {
		runner, ok := tgt.rt.Upstream.(growthTaskRunner)
		if !ok {
			a.taskProgressDone(st, tgt.a.UID, upstream.GrowthTaskReport{
				UID: tgt.a.UID, Nickname: tgt.a.Nickname,
				Err: "该渠道不支持成长任务",
			})
			continue
		}
		rep := runner.GrowthRunTasks(tgt.a, upstream.GrowthTaskOptions{
			IncludeMP: includeMP,
			Progress: func(done, total int, code string) {
				a.taskMu.Lock()
				st.current = fmt.Sprintf("%s · %s", tgt.a.Nickname, code)
				a.taskMu.Unlock()
			},
		})
		// 有到账（或本轮动过任务）就列入待刷新。
		if rep.Credit > 0 || rep.Energy > 0 {
			credited = append(credited, tgt.a.UID)
		}
		a.taskProgressDone(st, tgt.a.UID, rep)
	}
	a.refreshCreditsAfterTasks(credited)

	a.taskMu.Lock()
	st.running = false
	st.finishedAt = time.Now()
	st.current = ""
	total := st.accTotal
	var credit, energy int64
	for _, r := range st.reports {
		credit += r.Credit
		energy += r.Energy
	}
	a.taskMu.Unlock()
	log.Printf("task run done scope=%s accounts=%d credit=%d energy=%d refreshed=%d",
		st.scope, total, credit, energy, len(credited))
}

// refreshCreditsAfterTasks 任务跑完后刷新相关账号的积分余额。
//
// 为什么必须做：面板 `/api/state` 的 credits 读自 pool 缓存，
// 只有 `SetCreditDetail` 会改它。领奖已经真实到账，若不刷新，
// 用户看到的是「任务做完了但积分没变」—— 会误以为没领到。
//
// 在**后端**刷新（而非让前端调刷新接口）的原因：
// 无头/定时触发时没有页面；页面若中途关闭也会漏刷新。
func (a *App) refreshCreditsAfterTasks(uids []string) {
	for _, uid := range uids {
		if _, err := a.RefreshCredits(uid); err != nil {
			log.Printf("task run credit refresh failed uid=%s err=%v", uid, err)
		}
	}
}

// taskProgressDone 记录单账号结果并推进计数。
func (a *App) taskProgressDone(st *taskRunState, uid string, rep upstream.GrowthTaskReport) {
	a.taskMu.Lock()
	st.reports = append(st.reports, rep)
	st.accDone++
	a.taskMu.Unlock()
	if rep.Err != "" {
		log.Printf("task run account failed uid=%s err=%s", uid, rep.Err)
		return
	}
	log.Printf("task run account ok uid=%s tasks=%d credit=%d energy=%d",
		uid, len(rep.Tasks), rep.Credit, rep.Energy)
}

// taskTargets 解析执行范围。
//
// 跳过 Alias 渠道（共用同一账号池，如 TraeCode 复用 TraeWork 的池）与已禁用账号，
// 否则同一账号会被执行两次。
func (a *App) taskTargets(uid string) []taskTarget {
	a.mu.Lock()
	defer a.mu.Unlock()

	if strings.TrimSpace(uid) != "" {
		rt, au := a.findRuntimeAuth(uid)
		if rt == nil || au == nil {
			return nil
		}
		if !a.channelSupportsGrowth(rt.Kind) {
			return nil
		}
		return []taskTarget{{rt: rt, a: au}}
	}

	var out []taskTarget
	// 遍历顺序：按面板展示顺序（channelOrder），让账号执行顺序与用户看到的一致。
	seen := map[string]bool{}
	for _, kind := range a.orderedChannels() {
		rt := a.runtimes[kind]
		if rt == nil || rt.Alias || rt.Pool == nil {
			continue
		}
		// 跳过不支持成长中心的渠道：成长任务只有 WorkBuddy 国内版提供，
		// 其余渠道调用成长接口只会拿到 404，白跑一轮往返还污染日志。
		if !a.channelSupportsGrowth(kind) {
			continue
		}
		for _, s := range rt.Pool.List() {
			if s.Disabled || seen[s.UID] {
				continue
			}
			au := rt.Pool.AuthByUID(s.UID)
			if au == nil {
				continue
			}
			seen[s.UID] = true
			out = append(out, taskTarget{rt: rt, a: au})
		}
	}
	return out
}

// orderedChannels 按 channelOrder 权重返回渠道列表（未登记的排在末尾）。
func (a *App) orderedChannels() []provider.Kind {
	kinds := make([]provider.Kind, 0, len(a.runtimes))
	for k := range a.runtimes {
		kinds = append(kinds, k)
	}
	rank := func(k provider.Kind) int {
		if r, ok := channelOrder[k]; ok {
			return r
		}
		return 1 << 20
	}
	sort.Slice(kinds, func(i, j int) bool {
		ri, rj := rank(kinds[i]), rank(kinds[j])
		if ri != rj {
			return ri < rj
		}
		return kinds[i].String() < kinds[j].String()
	})
	return kinds
}

// TaskRunView 返回当前/最近一次任务执行的进度视图。
func (a *App) TaskRunView() taskRunView {
	a.taskMu.Lock()
	defer a.taskMu.Unlock()
	if a.taskRun == nil {
		return taskRunView{}
	}
	st := a.taskRun
	v := taskRunView{
		Running:       st.running,
		Scope:         st.scope,
		IncludeMP:     st.includeMP,
		AccountsTotal: st.accTotal,
		AccountsDone:  st.accDone,
		Current:       st.current,
		Reports:       append([]upstream.GrowthTaskReport(nil), st.reports...),
		Err:           st.errMsg,
	}
	if !st.startedAt.IsZero() {
		v.StartedAt = st.startedAt.Format(time.RFC3339)
	}
	if !st.finishedAt.IsZero() {
		v.FinishedAt = st.finishedAt.Format(time.RFC3339)
	}
	for _, r := range st.reports {
		v.Credit += r.Credit
		v.Energy += r.Energy
	}
	return v
}

// ---------------------------------------------------------------------------
// 成长中心开关（含热更新）
// ---------------------------------------------------------------------------

// SetGrowthConfig 保存成长中心自动化开关，并热更新到各渠道调度器。
//
// 热更新是必要的：调度器在构造时读一次配置，若只写文件不推送，
// 用户会觉得「开了开关却没生效」。
func (a *App) SetGrowthConfig(redeemStreak, travel bool, travelLocation int, tasks, tasksMP bool) error {
	if travelLocation < 0 || travelLocation > 4 {
		return fmt.Errorf("旅行目的地须为 0~4（0 视为 1），当前 %d", travelLocation)
	}
	a.mu.Lock()
	a.cfg.Schedule.GrowthRedeemStreak = redeemStreak
	a.cfg.Schedule.GrowthTravel = travel
	a.cfg.Schedule.GrowthTravelLocation = travelLocation
	a.cfg.Schedule.GrowthTasks = tasks
	a.cfg.Schedule.GrowthTasksIncludeMP = tasksMP
	err := config.Save(a.cfg, a.cfgPath)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	// 热更新：逐个调度器推送（各渠道独立开关）。
	for _, rt := range a.runtimes {
		if rt != nil && rt.Scheduler != nil {
			rt.Scheduler.SetGrowthOptions(redeemStreak, travel, travelLocation, tasks, tasksMP)
		}
	}
	log.Printf("成长中心开关已更新：兑换=%t 旅行=%t(%d) 任务=%t(含小程序=%t)",
		redeemStreak, travel, travelLocation, tasks, tasksMP)
	return nil
}

// GrowthConfigView 当前开关（面板回显）。
type GrowthConfigView struct {
	RedeemStreak   bool `json:"redeem_streak"`
	Travel         bool `json:"travel"`
	TravelLocation int  `json:"travel_location"`
	Tasks          bool `json:"tasks"`
	TasksIncludeMP bool `json:"tasks_include_mp"`
}

// GrowthConfig 读取当前成长中心开关。
func (a *App) GrowthConfig() GrowthConfigView {
	a.mu.Lock()
	defer a.mu.Unlock()
	return GrowthConfigView{
		RedeemStreak:   a.cfg.Schedule.GrowthRedeemStreak,
		Travel:         a.cfg.Schedule.GrowthTravel,
		TravelLocation: a.cfg.Schedule.GrowthTravelLocation,
		Tasks:          a.cfg.Schedule.GrowthTasks,
		TasksIncludeMP: a.cfg.Schedule.GrowthTasksIncludeMP,
	}
}

// GrowthSupported 报告是否存在支持成长任务的渠道（用于面板禁用按钮）。
func (a *App) GrowthSupported() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, rt := range a.runtimes {
		if rt == nil || rt.Alias {
			continue
		}
		if _, ok := rt.Upstream.(growthTaskRunner); ok {
			return true
		}
	}
	return false
}

// channelSupportsGrowth 报告某个渠道是否支持成长任务（面板按账号卡片显示入口）。
//
// 与 accountGroup / findRuntimeAuth 同一约定：a.runtimes 构造后只读，无需加锁。
// 若改成需要加锁，注意 GetState 的调用路径（先解锁再构建 State）。
func (a *App) channelSupportsGrowth(kind provider.Kind) bool {
	rt := a.runtimes[kind]
	if rt == nil || rt.Alias {
		return false
	}
	if isGrowthUnsupportedChannel(kind) {
		return false
	}
	_, ok := rt.Upstream.(growthTaskRunner)
	return ok
}

// isGrowthUnsupportedChannel 成长中心的渠道黑名单。
//
// 成长中心**仅 WorkBuddy 国内版**提供：其它渠道调用成长接口会得到 404
// 或「接口不存在」。显式排除可避免面板显示一个必然失败的按钮。
func isGrowthUnsupportedChannel(kind provider.Kind) bool {
	return kind != provider.WorkBuddy
}

// RunGrowthTravel 立即对单个账号执行一次猫猫旅行（面板账号行上的 🐱 按钮）。
//
// ## 为什么不受 schedule.growth_travel 开关约束
//
// 那个开关管的是「定时自动跑」；用户手动点了按钮就是明确要执行这一次。
// 若还要求先开开关，按钮就是个摆设——这正是「找不到在哪执行」的由来。
// 目的地仍沿用配置里的 travel_location（0/越界视为 1），保证手动与自动落到同一处。
//
// ## 为什么同步返回
//
// 旅行只有 1~2 次上游往返（查状态 → 领取 或 派出），亚秒级；不像成长任务
// 要几十个请求那样必须异步轮询。同步能让用户立刻看到「已派出 / 在途 / 到站领取 +N」。
//
// ## 失败不罚号
//
// 这里只是替用户点一下按钮。账号健康由会话失效/限流路径负责，不在此处改冷却。
func (a *App) RunGrowthTravel(uid string) (string, error) {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return "", errors.New("账号不能为空")
	}
	a.mu.Lock()
	rt, au := a.findRuntimeAuth(uid)
	loc := a.cfg.Schedule.GrowthTravelLocation
	a.mu.Unlock()
	if rt == nil || au == nil {
		return "", fmt.Errorf("账号不存在：%s", uid)
	}
	tr, ok := rt.Upstream.(growthTraveler)
	if !ok {
		return "", fmt.Errorf("渠道 %s 不支持猫猫旅行（成长中心目前仅 WorkBuddy 国内版提供）", rt.Kind)
	}
	if loc < 1 || loc > 4 {
		loc = 1
	}
	msg, err := tr.GrowthTravelOnce(au, loc)
	if err != nil {
		log.Printf("growth travel manual failed platform=%s uid=%s err=%v", rt.Kind, uid, err)
		return "", err
	}
	log.Printf("growth travel manual ok platform=%s uid=%s loc=%d msg=%s", rt.Kind, uid, loc, msg)
	// 到站领取会加积分。不刷新的话面板还显示旧余额，用户会以为没领到
	// ——与 refreshCreditsAfterTasks 同一个理由。
	if _, e := a.RefreshCredits(uid); e != nil {
		log.Printf("growth travel credit refresh failed uid=%s err=%v", uid, e)
	}
	return msg, nil
}
