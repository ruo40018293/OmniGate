// growth_auto.go 成长中心的**单账号编排**：连登兑换+抽奖、猫猫旅行。
//
// 与 growth.go 的分工：
//
//	growth.go       协议层 —— 一个函数对应一个上游端点，不做流程编排
//	growth_auto.go  编排层 —— 把多个端点串成一个「可安全重复执行」的日常动作
//
// ## 为什么这两个动作可以自动跑（而「任务一键完成」要谨慎）
//
// 兑换与旅行是**真实的服务端动作**，不是事件伪造：
//   - 兑换：按档位状态调用，未解锁由服务端拒绝（403），天然幂等
//   - 抽奖：次数只能从兑换获得，用掉即减少 —— 故有单次上限保护
//   - 旅行：每自然日仅可派一次（上游 daily_limit_reached 控制）
//
// 对比之下，「17 个成长任务」靠的是**伪造客户端行为事件**，
// 载荷形状依赖上游实现细节，变更风险高，因此不在本文件的自动化范围内。
//
// ## 调用前提
//
// 调用方必须先确认该渠道支持成长中心（当前仅 workbuddy 国内版）。
// 其它渠道调用会得到 404/400，不应视为账号故障。
package upstream

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"omnigate/internal/auth"
)

// maxLotteryDrawPerRun 单次编排最多抽奖次数。
//
// 安全阀：抽奖次数来自兑换，理论上个位数；但若上游数值异常
// （例如误返回 9999），无上限循环会瞬间打光并持续轰炸上游。
// 超出部分留到下次执行，不影响正确性。
const maxLotteryDrawPerRun = 30

// GrowthRedeemAndDraw 兑换已解锁的连登档位，并把抽奖次数抽完。
//
// 返回 (已兑换档位数, 已抽奖次数, 人类可读摘要, 错误)。
//
// 语义要点：
//   - 只对**已解锁**档位发起兑换；未解锁时服务端返回 403，属预期状态，跳过
//   - 兑换可能新增抽奖次数，因此**兑换后再查一次**次数
//   - 抽奖有单次上限（maxLotteryDrawPerRun），防止上游数值异常时死循环
//   - 全流程可安全重复执行：已兑换档位不会被重复发放
func (c *Client) GrowthRedeemAndDraw(a *auth.Auth) (redeemed, drawn int, msg string, err error) {
	st, err := c.GrowthStreakFull(a)
	if err != nil {
		return 0, 0, "", fmt.Errorf("查询连登状态失败: %w", err)
	}

	var parts []string
	parts = append(parts, fmt.Sprintf("连登 %d 天", st.Days))

	// 逐档位尝试兑换
	tiers := []struct {
		id     string
		status string
	}{
		{"7d", st.Tier7dStatus},
		{"14d", st.Tier14dStatus},
		{"28d", st.Tier28dStatus},
	}
	var redeemErrs []string
	for _, t := range tiers {
		if !tierRedeemable(t.status) {
			continue
		}
		if e := c.GrowthRedeemTier(a, t.id); e != nil {
			if isForbidden(e) {
				// 403 = 状态与实际不同步（刚解锁/刚被领），属预期，跳过
				continue
			}
			redeemErrs = append(redeemErrs, fmt.Sprintf("%s: %v", t.id, e))
			continue
		}
		redeemed++
	}
	if redeemed > 0 {
		parts = append(parts, fmt.Sprintf("兑换 %d 档", redeemed))
	}

	// 兑换后再查抽奖次数（兑换是次数的来源）
	chances, enabled, cerr := c.LotteryChances(a)
	switch {
	case cerr != nil:
		// 抽奖查询失败不影响兑换结果，仅记录
		parts = append(parts, "抽奖次数查询失败")
	case !enabled:
		// 活动未启用，属正常
	case chances <= 0:
		// 无次数，正常
	default:
		n := chances
		if n > maxLotteryDrawPerRun {
			n = maxLotteryDrawPerRun
		}
		var drawErrs []string
		for i := 0; i < n; i++ {
			if _, e := c.LotteryDraw(a); e != nil {
				drawErrs = append(drawErrs, e.Error())
				break
			}
			drawn++
		}
		if drawn > 0 {
			parts = append(parts, fmt.Sprintf("抽奖 %d 次", drawn))
		}
		if len(drawErrs) > 0 {
			parts = append(parts, "抽奖中断")
		}
	}

	if len(redeemErrs) > 0 {
		return redeemed, drawn, strings.Join(parts, "，"),
			fmt.Errorf("兑换部分失败: %s", strings.Join(redeemErrs, "; "))
	}
	return redeemed, drawn, strings.Join(parts, "，"), nil
}

// tierRedeemable 判定档位状态是否可兑换。
//
// 上游状态字符串实测有 "redeemable" / "locked" / "redeemed" 等；
// 这里只放行明确可兑换的，其余（含未知值）一律跳过 ——
// 宁可漏领也不要对着上游反复试探。
func tierRedeemable(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "redeemable", "available", "unlocked":
		return true
	default:
		return false
	}
}

// isForbidden 判定 403（未解锁兑换）。
func isForbidden(err error) bool {
	var ue *Error
	return asError(err, &ue) && ue.Status == http.StatusForbidden
}

// GrowthTravelOnce 猫猫旅行：到站则领取，空闲则派出，在途则跳过。
//
// 返回人类可读摘要与错误。该动作可安全重复执行：
// 每自然日仅能派出一次，由上游 daily_limit_reached 控制。
func (c *Client) GrowthTravelOnce(a *auth.Auth, locationID int) (string, error) {
	st, err := c.TravelStatus(a)
	if err != nil {
		return "", fmt.Errorf("查询旅行状态失败: %w", err)
	}

	switch strings.ToLower(strings.TrimSpace(st.State)) {
	case "arrived":
		credit, e := c.TravelClaim(a, st.RecordID)
		if e != nil {
			return "", fmt.Errorf("领取到站奖励失败: %w", e)
		}
		return fmt.Sprintf("到站领取 +%d", credit), nil

	case "traveling":
		return "在途", nil

	case "idle", "":
		if st.DailyLimitReached {
			return "今日已派出", nil
		}
		if e := c.TravelDepart(a, locationID); e != nil {
			return "", fmt.Errorf("派出失败: %w", e)
		}
		return "已派出", nil

	default:
		// 未知状态：不动作，避免误判造成重复派出
		return fmt.Sprintf("未知状态 %q，跳过", st.State), nil
	}
}

// IsGrowthUnsupported 判定「该渠道不支持成长中心」类错误。
//
// 现状：成长中心仅 WorkBuddy 国内版提供。其它渠道调用会得到 404，
// 或业务层「接口不存在」。调用方据此**静默跳过**，不要记为账号故障 ——
// 否则会把健康账号误冷却。
func IsGrowthUnsupported(err error) bool {
	if err == nil {
		return false
	}
	var ue *Error
	if !asError(err, &ue) {
		return false
	}
	if ue.Status == http.StatusNotFound {
		return true
	}
	if ue.Status == http.StatusBadRequest || ue.Status == http.StatusForbidden {
		m := strings.ToLower(ue.Msg)
		return strings.Contains(m, "not found") ||
			strings.Contains(m, "not support") ||
			strings.Contains(m, "unsupported")
	}
	return false
}

// IsGrowthUnsupported 方法形式：供调度器通过可选接口探测，
// 避免调度器 import 本包。语义与包级函数一致。
func (c *Client) IsGrowthUnsupported(err error) bool { return IsGrowthUnsupported(err) }

// IsGrowthTransient 判定可重试的瞬时错误（网络/5xx/限流）。
//
// 与 IsGrowthUnsupported 配合使用：不可重试的错误当日不再尝试，
// 可重试的留给下次定时点。
func IsGrowthTransient(err error) bool {
	if err == nil {
		return false
	}
	var ue *Error
	if !asError(err, &ue) {
		// 非上游业务错误（网络层）一律视为瞬时
		return true
	}
	return ue.Status == http.StatusTooManyRequests || ue.Status >= 500
}

// 供调用方判断错误是否应中止本轮（而非继续下一个账号）。
var errGrowthStop = errors.New("growth: stop")

// ShouldStopGrowth 报告该错误是否应中止本轮所有账号的处理。
// 当前规则：仅「渠道不支持」需要中止（后续账号必然同样失败）。
func ShouldStopGrowth(err error) bool {
	return IsGrowthUnsupported(err) || errors.Is(err, errGrowthStop)
}
