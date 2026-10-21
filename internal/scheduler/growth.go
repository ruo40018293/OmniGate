// growth.go 调度侧的成长中心自动化接入。
//
// ## 设计取舍：可选接口探测，不 import 具体渠道包
//
// 成长中心只有部分渠道提供（当前仅 WorkBuddy 国内版）。若在 scheduler 里
// 直接 import upstream 并断言类型，会让调度器与某个渠道实现硬耦合，
// 新增/移除渠道都要改调度器。
//
// 因此这里沿用本项目既有的「接口探测」手法：定义一个**最小接口**，
// 由支持该能力的渠道实现；调度器只做 `errors.As` 式探测，探测失败即跳过。
//
// 接口方法刻意只返回**基本类型**（int / string / error），
// 不暴露渠道包里的结构体，从而保持零依赖。
//
// ## 安全前提：默认关闭
//
// 这两项自动化会**真实改变账号状态**（兑换档位、消耗抽奖次数、派出旅行），
// 与签到这类幂等动作不同 —— 抽奖次数用掉不可撤销。
// 故由配置项控制且**默认关闭**，必须用户显式开启。
package scheduler

import (
	"log"
	"strings"
	"time"

	"omnigate/internal/auth"
)

// growthAutomator 由支持成长中心的渠道实现（当前仅 workbuddy）。
//
// 返回的 msg 用于日志，便于用户从日志复原「这次做了什么」。
type growthAutomator interface {
	// GrowthRedeemAndDraw 兑换已解锁的连登档位并抽完抽奖次数。
	// 返回 (已兑换档位数, 已抽奖次数, 摘要, 错误)。
	GrowthRedeemAndDraw(a *auth.Auth) (redeemed, drawn int, msg string, err error)
	// GrowthTravelOnce 派出或领取猫猫旅行，返回摘要与错误。
	GrowthTravelOnce(a *auth.Auth, locationID int) (msg string, err error)
}

// growthSkipper 可选实现：渠道用于告知某错误是否应中止本轮（如渠道不支持）。
type growthSkipper interface {
	IsGrowthUnsupported(err error) bool
}

// growthAutomator 探测。返回 nil 表示该渠道不支持成长中心。
func (s *Scheduler) growthAutomator() growthAutomator {
	ga, _ := s.cfg.Upstream.(growthAutomator)
	return ga
}

// growthEnabled 报告是否配置了任一项成长自动化。
func (s *Scheduler) growthEnabled() bool {
	o := s.growthOptions()
	return o.redeemStreak || o.travel
}

// RunGrowthNow 立即对所有账号执行成长中心自动化（受配置开关约束）。
//
// 由签到调度在每次签到批次后调用；也供面板「立即执行」使用。
// 未启用任何开关时**不做任何网络请求**。
func (s *Scheduler) RunGrowthNow() {
	if !s.growthEnabled() {
		return
	}
	ga := s.growthAutomator()
	if ga == nil {
		// 该渠道不支持成长中心：静默跳过（不是错误，别刷日志）
		log.Printf("growth skip platform=%s reason=channel-unsupported", s.name())
		return
	}

	name := s.name()
	o := s.growthOptions()
	accounts := s.cfg.Pool.List()
	log.Printf("growth batch start platform=%s accounts=%d redeem=%t travel=%t",
		name, len(accounts), o.redeemStreak, o.travel)

	skip := growthSkipper(nil)
	if sk, ok := s.cfg.Upstream.(growthSkipper); ok {
		skip = sk
	}

	for _, st := range accounts {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		s.runGrowthFor(ga, skip, a)
	}
	log.Printf("growth batch done platform=%s", name)
}

// runGrowthFor 对单个账号执行已启用的成长动作。
//
// 两个动作各自独立：一个失败不影响另一个（用户可能只想开其中一个）。
func (s *Scheduler) runGrowthFor(ga growthAutomator, skip growthSkipper, a *auth.Auth) {
	name := s.name()

	o := s.growthOptions()
	if o.redeemStreak {
		redeemed, drawn, msg, err := ga.GrowthRedeemAndDraw(a)
		switch {
		case err != nil:
			if skip != nil && skip.IsGrowthUnsupported(err) {
				log.Printf("growth redeem platform=%s uid=%s 渠道不支持，跳过", name, a.UID)
			} else {
				log.Printf("growth redeem failed platform=%s uid=%s redeemed=%d drawn=%d msg=%s err=%v",
					name, a.UID, redeemed, drawn, msg, err)
			}
		default:
			log.Printf("growth redeem ok platform=%s uid=%s redeemed=%d drawn=%d msg=%s",
				name, a.UID, redeemed, drawn, msg)
		}
	}

	if o.travel {
		msg, err := ga.GrowthTravelOnce(a, s.travelLocationID())
		if err != nil {
			if skip != nil && skip.IsGrowthUnsupported(err) {
				log.Printf("growth travel platform=%s uid=%s 渠道不支持，跳过", name, a.UID)
			} else {
				log.Printf("growth travel failed platform=%s uid=%s err=%v", name, a.UID, err)
			}
		} else {
			log.Printf("growth travel ok platform=%s uid=%s msg=%s", name, a.UID, msg)
		}
	}
}

// travelLocationID 返回收敛后的旅行目的地（1~4）。0 或越界一律取 1。
func (s *Scheduler) travelLocationID() int {
	loc := s.growthOptions().travelLocation
	if loc < 1 || loc > 4 {
		return 1
	}
	return loc
}

// runGrowthOncePerDay 当日首次命中配置签到时刻时，跟随执行成长自动化。
//
// 三重约束，避免误触发或重复消耗：
//  1. 未启用开关 → 直接返回（不发任何请求）
//  2. 只在命中配置时刻调用（窗口重试的每分钟不调用）
//  3. 当日只跑一次（growthDoneDay 标记）
//
// 异步执行：一轮可能涉及多账号 × 多次请求，同步跑会阻塞定时器循环，
// 导致后续签到时刻被推迟。
func (s *Scheduler) runGrowthOncePerDay(now time.Time) {
	if !s.growthEnabled() {
		return
	}
	day := now.Format("2006-01-02")
	s.retryMu.Lock()
	if s.growthDoneDay == day {
		s.retryMu.Unlock()
		return
	}
	s.growthDoneDay = day
	s.retryMu.Unlock()

	s.safeGo(func() { s.RunGrowthNow() })
}

// safeGo 起一个带 panic 兜底的 goroutine。
//
// 成长自动化是「旁路」任务：它的任何异常都不应影响签到主链路，
// 因此这里 recover 后仅记日志，不上抛。
func (s *Scheduler) safeGo(fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("growth goroutine panic platform=%s: %v", s.name(), r)
			}
		}()
		fn()
	}()
}

// growthMaxRuntime 单轮成长自动化的兜底时长上限（仅用于日志提示，
// 实际的网络超时由 Upstream 侧的 http.Client 控制）。
const growthMaxRuntime = 5 * time.Minute

// ---------------------------------------------------------------------------
// 成长任务「一键完成」（风险最高，默认关闭）
// ---------------------------------------------------------------------------

// growthTasksRunner 由支持成长任务的渠道实现（当前仅 workbuddy 国内版）。
//
// 返回**基本类型**而非渠道包里的报告结构 —— 与 growthAutomator 同样的解耦约定：
// 调度器不 import 具体渠道包，只看这一组「能不能跑、跑了什么」的信息。
type growthTasksRunner interface {
	// GrowthRunTasksOnce 对单个账号执行任务动作表，返回摘要与到账积分/能量。
	// includeMP 决定是否包含小程序口径任务。
	GrowthRunTasksOnce(a *auth.Auth, includeMP bool) (msg string, credit, energy int64, err error)
	// GrowthTasksSupported 报告该渠道是否提供成长中心（否则静默跳过）。
	GrowthTasksSupported() bool
}

// growthTasksRunner 探测。返回 nil 表示该渠道不支持。
func (s *Scheduler) growthTasksRunner() growthTasksRunner {
	gr, _ := s.cfg.Upstream.(growthTasksRunner)
	return gr
}

// growthTasksEnabled 报告是否配置了成长任务自动执行。
func (s *Scheduler) growthTasksEnabled() bool { return s.growthOptions().tasks }

// runGrowthTasksOncePerDay 当日首次命中配置签到时刻时，跟随执行成长任务。
//
// 与 runGrowthOncePerDay 分开实现（各自独立开关、独立日标记）：
// 用户可能只想要其中一项，共用标记会让先触发的一方吃掉另一方的执行机会。
func (s *Scheduler) runGrowthTasksOncePerDay(now time.Time) {
	if !s.growthTasksEnabled() {
		return
	}
	day := now.Format("2006-01-02")
	s.retryMu.Lock()
	if s.growthTasksDoneDay == day {
		s.retryMu.Unlock()
		return
	}
	s.growthTasksDoneDay = day
	s.retryMu.Unlock()

	s.safeGo(func() { s.RunGrowthTasksNow() })
}

// RunGrowthTasksNow 立即对所有账号执行成长任务动作表（受配置开关约束）。
//
// 未开启开关时**不做任何网络请求**。单账号失败不中断整批。
func (s *Scheduler) RunGrowthTasksNow() {
	if !s.growthTasksEnabled() {
		return
	}
	gr := s.growthTasksRunner()
	if gr == nil || !gr.GrowthTasksSupported() {
		log.Printf("growth tasks skip platform=%s reason=channel-unsupported", s.name())
		return
	}

	name := s.name()
	incMP := s.growthOptions().tasksMP
	accounts := s.cfg.Pool.List()
	log.Printf("growth tasks batch start platform=%s accounts=%d include_mp=%t", name, len(accounts), incMP)

	for _, st := range accounts {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		msg, credit, energy, err := gr.GrowthRunTasksOnce(a, incMP)
		if err != nil {
			if IsGrowthUnsupportedErr(err) {
				log.Printf("growth tasks platform=%s uid=%s 渠道不支持，跳过", name, a.UID)
				return // 后续账号必然同样失败，整轮中止
			}
			log.Printf("growth tasks failed platform=%s uid=%s err=%v", name, a.UID, err)
			continue
		}
		log.Printf("growth tasks ok platform=%s uid=%s credit=%d energy=%d msg=%s",
			name, a.UID, credit, energy, msg)
	}
	log.Printf("growth tasks batch done platform=%s", name)
}

// IsGrowthUnsupportedErr 判断错误是否为「渠道不支持成长中心」。
//
// 调度器不 import 渠道包，故这里按错误**文本**做保守判断：只有明确的
// 404 / not found / not support 才算，其余一律当作可继续的单账号失败。
func IsGrowthUnsupportedErr(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "404") ||
		strings.Contains(m, "not found") ||
		strings.Contains(m, "not support") ||
		strings.Contains(m, "unsupported")
}
