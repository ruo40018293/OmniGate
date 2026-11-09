// catchup.go 「今日漏跑就补跑」的兜底逻辑。
//
// ## 为什么需要
//
// 2026-11-08 用户反馈「签到没用了、旅行没自动跑」。查日志发现两件不同性质的事：
//
//  1. 猫猫旅行：09:00 那个批次**确实自动派出**了（`growth travel ok msg=已派出`），
//     但「派出」与「到站领取」是两个动作，而自动派只跑一次 —— 到站时没有第二次
//     自动调度，用户必须自己点面板上的 🐱 才能领到那 10 分。
//  2. 签到：只要 daemon 在配置时刻（09:00/21:00）**没在运行**（关机、休眠、
//     刚装好、崩溃重启过），那一天整个时段就再没有触发点，`checkinDone` 也是
//     纯内存的、重启即清空，等于整天漏领。
//
// 本文件解决的是「**已经过了配置时刻，但今天还没成功过**」这一类漏跑。
//
// ## 判定依据必须是持久化状态
//
// `checkinDone` 与 `growthDoneDay` 都在内存里，重启就丢 —— 用它们判「今天做过
// 没有」会把「重启前的成功」忘光，重启后重复签到（幂等但白打上游），
// 或者反过来永远补不上。故这里的判定读 `pool.Status.LastCheckinAt/LastCheckinOK`
// （落盘在 state-*.json），是与进程生命周期无关的事实。
//
// ## 只补「首次」，不接管后续
//
// 补跑的时机是：Run 循环启动时（进程刚起来）+ 每次定时器唤醒时（覆盖跨零点、
// 长时间休眠后醒来）。一旦本进程已经为今天做过判断，就不再重复补跑（
// `catchupDoneDay`），把后续交给正常的配置时刻与窗口重试。
package scheduler

import (
	"log"
	"time"
)

// catchupGrace 补跑保护期：配置时刻过后多久才认为「确实漏了」。
//
// 不设这个窗口的话，进程在 09:00:00 前几毫秒启动会立刻判定「今天没签到」并补跑，
// 与紧接着的整点触发撞车（虽然幂等，但会多打一次上游，日志也更难读）。
const catchupGrace = 2 * time.Minute

// catchUpToday 检查今天是否还有该做而没做的事，缺什么补什么。
//
// 由 Run 在**每次进入等待前**调用（启动 + 每次唤醒）。内部用内存标记
// catchupDoneDay 保证每天只判定一次，重复调用几乎零成本。
func (s *Scheduler) catchUpToday(now time.Time) {
	day := now.Format("2006-01-02")

	s.retryMu.Lock()
	if s.catchupDoneDay == day {
		s.retryMu.Unlock()
		return
	}
	s.catchupDoneDay = day
	s.retryMu.Unlock()

	s.catchUpCheckin(now)
	s.catchUpGrowth(now)
}

// catchUpCheckin 今天没有任何账号成功签到、且已过首个配置时刻 ⇒ 立刻补跑。
//
// 不补的情形（每一条都有理由）：
//   - 没有签到任务（CheckinMinutes 为空）：本来就不该签到。
//   - ActivitiesOnly（如 WorkBuddy 国际版）：签到只是活跃保活，
//     且**不落盘签到状态**，按状态判定永远为「没做过」⇒ 会无限补跑。
//   - 还没到首个配置时刻：交给正常调度，不算漏。
//   - 今天已经有人成功过：正常路径在跑，补跑只会重复请求。
func (s *Scheduler) catchUpCheckin(now time.Time) {
	ch := s.CheckinMinutes()
	if len(ch) == 0 || s.cfg.ActivitiesOnly {
		return
	}
	first := ch[0]
	if now.Hour()*60+now.Minute() < first+int(catchupGrace/time.Minute) {
		return
	}
	if s.anyCheckinSucceededToday(now) {
		return
	}
	log.Printf("checkin catchup platform=%s reason=今日 %02d:%02d 后无成功签到记录（进程可能未在该时刻运行）",
		s.name(), first/60, first%60)
	s.safeGo(func() { s.runCheckin(first) })
}

// catchUpGrowth 今天成长自动化还没自动跑过、且已过首个配置时刻 ⇒ 补跑一次。
//
// 旅行尤其需要它：「派出」与「到站领取」是同一个方法里的两个分支，靠
// TravelStatus 的状态机推进。只跑一次的结果是 —— 派出成功了，到站的 10 分
// 却要等用户手点。上游 `GrowthTravelOnce` 本身是幂等的（每日仅能派出一次，
// 由 daily_limit_reached 控制），重复调用只会返回「今日已派出」或「已领取」。
func (s *Scheduler) catchUpGrowth(now time.Time) {
	if !s.growthEnabled() && !s.growthTasksEnabled() {
		return
	}
	ch := s.CheckinMinutes()
	// 成长自动化的调度点是「当日首个签到时刻」，没有签到时刻就无从谈起。
	if len(ch) == 0 {
		return
	}
	first := ch[0]
	if now.Hour()*60+now.Minute() < first+int(catchupGrace/time.Minute) {
		return
	}
	log.Printf("growth catchup platform=%s reason=今日成长自动化尚未执行", s.name())
	s.runGrowthOncePerDay(now)
	s.runGrowthTasksOncePerDay(now)
}

// anyCheckinSucceededToday 今天是否已有账号成功签到。
//
// 判定读持久化的 LastCheckinAt + LastCheckinOK：
//   - LastCheckinAt 为空 = 从未签到过（新导入的账号）⇒ 今天就该补。
//   - 时间落在今天且 OK = 今天成功过 ⇒ 不用补。
//   - 时间落在今天但 OK=false = 今天试过但失败了 ⇒ 不在这里补，
//     交给窗口重试与按账号的限流延迟重试，避免与它们重复打上游。
//
// 只要求**有一个**账号成功即可：`runCheckin` 是按批次标记时段完成的，
// 部分账号失败属于账号自身问题（无活动/限流），不该拖累整批反复补跑。
func (s *Scheduler) anyCheckinSucceededToday(now time.Time) bool {
	day := now.Format("2006-01-02")
	for _, st := range s.cfg.Pool.List() {
		if st.LastCheckinOK && st.LastCheckinAt.Format("2006-01-02") == day {
			return true
		}
	}
	return false
}
