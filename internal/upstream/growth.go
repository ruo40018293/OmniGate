// growth.go 成长中心（growth）接口：任务列表/接受/领奖、连登兑换、抽奖、猫猫旅行、Buddy。
//
// ## 域划分（这是本文件最容易搞错的地方）
//
// 成长中心的接口**分布在两个域上**，混用会得到「路径不存在」类的 400：
//
//	chatBase (copilot.tencent.com)  任务列表 / accept / streak / lottery / buddy / travel
//	webBase  (www.workbuddy.cn)     任务**领奖**（任务码在 URL 路径里）
//
// 历史上曾用 CLI 域调 /v2/activity/growth/tasks/reward/claim（任务码放 body），
// 该路径**不存在**，恒返回 400 "task not completed" —— 这是领奖一直失败的真实原因。
//
// ## 幂等性
//
//   - accept 是「报名」，不产生进度；进度由服务端行为事件点亮（如 chat_request_send
//     上报、真实对话），因此 accept 可安全重放
//   - claim 仅在进度达标后可领；重复领返回 already_claimed=true，不算错误
//   - redeem（连登兑换）带 client_token 幂等令牌；未解锁返回 403
//
// 本文件只负责协议层（纯 API 调用 + 解析），不含任何编排逻辑。
package upstream

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"omnigate/internal/auth"
)

// growth 域路径（实测口径）。
const (
	tasksListPath   = "/v2/activity/growth/tasks"
	tasksAcceptPath = "/v2/activity/growth/tasks/accept"

	streakPath         = "/activity/growth/streak"
	streakRedeemPath   = "/activity/growth/redeem"
	lotterySummaryPath = "/activity/growth/lottery/summary"
	lotteryDrawPath    = "/activity/growth/lottery/draw"

	travelStatusPath = "/activity/growth/buddy/travel/status"
	travelDepartPath = "/activity/growth/buddy/travel/depart"
	travelClaimPath  = "/activity/growth/buddy/travel/claim"
	buddyInfoPath    = "/activity/growth/buddy/info"
	buddyFirstPath   = "/activity/growth/buddy/first"
	buddyAgreePath   = "/activity/growth/buddy/agreement"
)

// buddyTaskIncompleteMarker 领养门槛未达标的业务错误关键词（HTTP 400 时出现）。
// 该状态当日不应重试 —— 上游要求先完成前置任务。
const buddyTaskIncompleteMarker = "first_buddy task not completed yet"

// mpPlatformHeader / mpPlatform 小程序口径：小程序限定任务的列表下发、accept、
// claim 全链路都要求 X-Client-Platform: miniprogram（缺头时 accept 返回 task not found）。
const (
	mpPlatformHeader = "X-Client-Platform"
	mpPlatform       = "miniprogram"
)

// clientToken 生成幂等令牌（与前端 randomUUID 同语义）。
func clientToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "00000000-0000-0000-0000-000000000000"
	}
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]), hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]), hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]))
}

// growthJSON 发 growth 域请求（chatBase + BillingHeaders）并解信封。
func (c *Client) growthJSON(a *auth.Auth, method, path string, body any) (json.RawMessage, error) {
	return c.growthJSONAt(c.chatBase(a), a, method, path, body, "")
}

// growthJSONMP 同 growthJSON，但叠加小程序口径头。
func (c *Client) growthJSONMP(a *auth.Auth, method, path string, body any) (json.RawMessage, error) {
	return c.growthJSONAt(c.chatBase(a), a, method, path, body, mpPlatform)
}

// growthJSONWeb 发 Web 域 growth 请求（webBase + Web 端请求头形状）。
func (c *Client) growthJSONWeb(a *auth.Auth, method, path string, body any) (json.RawMessage, error) {
	return c.growthJSONAt(c.webBase(a), a, method, path, body, "web")
}

// growthJSONAt 统一实现：base + 路径 + 可选 X-Client-Platform 头。
//
// platform 取值：
//
//	""           默认口径（chatBase + BillingHeaders）
//	"web"        Web 域形状：Origin/Referer 指向成长中心，走 web 头
//	"miniprogram" 小程序口径
func (c *Client) growthJSONAt(base string, a *auth.Auth, method, path string, body any, platform string) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if platform == "web" {
		// Web 端请求头形状（对照浏览器实际请求）
		req.Header.Set("Origin", "https://www.workbuddy.cn")
		req.Header.Set("Referer", "https://www.workbuddy.cn/profile/growth-center")
		req.Header.Set("x-client-platform", "web")
	} else if platform != "" {
		req.Header.Set(mpPlatformHeader, platform)
	}
	if c.userAgentForGrowth() != "" {
		req.Header.Set("User-Agent", c.userAgentForGrowth())
	}
	return c.doJSON(req)
}

// userAgentForGrowth 成长域 UA。基线未对外暴露 UA 配置时返回空串（不带该头）。
func (c *Client) userAgentForGrowth() string { return c.GrowthUA }

// ---------------------------------------------------------------------------
// 任务：列表 / 接受 / 领奖
// ---------------------------------------------------------------------------

// Task 单个任务的对外视图（字段名与上游 JSON 对齐，多余字段不透出）。
type Task struct {
	TaskCode     string `json:"task_code"`
	Title        string `json:"title,omitempty"`
	Description  string `json:"description,omitempty"`
	TaskDesc     string `json:"task_desc,omitempty"`
	Credit       int64  `json:"credit,omitempty"`
	Energy       int64  `json:"energy,omitempty"`
	HasReward    bool   `json:"has_reward,omitempty"`
	RewardBuddy  bool   `json:"reward_buddy,omitempty"`
	TaskType     string `json:"task_type,omitempty"`
	Tag          string `json:"tag,omitempty"`
	JumpURL      string `json:"jump_url,omitempty"`
	Locked       bool   `json:"locked,omitempty"`
	Target       int64  `json:"target"`
	Current      int64  `json:"current"`
	AcceptStatus string `json:"accept_status,omitempty"`
	Status       string `json:"status,omitempty"`
	Claimable    bool   `json:"claimable,omitempty"`
	Claimed      bool   `json:"claimed,omitempty"`
}

// ListTasks 拉取全量任务列表（默认口径）。
func (c *Client) ListTasks(a *auth.Auth) ([]Task, error) {
	data, err := c.growthJSON(a, http.MethodGet, tasksListPath, nil)
	if err != nil {
		return nil, err
	}
	return parseGrowthTasks(data)
}

// ListTasksMP 拉取小程序口径的任务列表。
//
// ⚠️ 实测 mp 列表是**默认口径的超集**（含 RichMeow / Model_chat 等常规任务 +
// 小程序专属任务），调用方合并时须按 task_code 去重。
func (c *Client) ListTasksMP(a *auth.Auth) ([]Task, error) {
	data, err := c.growthJSONMP(a, http.MethodGet, tasksListPath, nil)
	if err != nil {
		return nil, err
	}
	return parseGrowthTasks(data)
}

// parseGrowthTasks 解析 data.tasks[]（默认与 mp 口径共用）。
//
// progress 字段有两种形态：{current,target} 对象，或平铺的 current/target。
// 两者都尝试，对象形态优先。
func parseGrowthTasks(data json.RawMessage) ([]Task, error) {
	var resp struct {
		Tasks []struct {
			TaskCode     string          `json:"task_code"`
			Title        string          `json:"title"`
			Description  string          `json:"description"`
			TaskDesc     string          `json:"task_desc"`
			RewardCredit int64           `json:"reward_credit"`
			RewardEnergy int64           `json:"reward_energy"`
			HasReward    bool            `json:"has_reward"`
			RewardBuddy  bool            `json:"reward_buddy"`
			TaskType     string          `json:"task_type"`
			Tag          string          `json:"tag"`
			JumpURL      string          `json:"jump_url"`
			Locked       bool            `json:"locked"`
			AcceptStatus string          `json:"accept_status"`
			Status       string          `json:"status"`
			Target       int64           `json:"target"`
			Current      int64           `json:"current"`
			Progress     json.RawMessage `json:"progress"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	out := make([]Task, 0, len(resp.Tasks))
	for _, t := range resp.Tasks {
		cur, tgt := t.Current, t.Target
		if len(t.Progress) > 0 && string(t.Progress) != "null" {
			var pr struct {
				Current int64 `json:"current"`
				Target  int64 `json:"target"`
			}
			if json.Unmarshal(t.Progress, &pr) == nil && (pr.Target > 0 || pr.Current > 0) {
				cur, tgt = pr.Current, pr.Target
			}
		}
		claimed := t.AcceptStatus == "claimed"
		out = append(out, Task{
			TaskCode:     t.TaskCode,
			Title:        t.Title,
			Description:  t.Description,
			TaskDesc:     t.TaskDesc,
			Credit:       t.RewardCredit,
			Energy:       t.RewardEnergy,
			HasReward:    t.HasReward,
			RewardBuddy:  t.RewardBuddy,
			TaskType:     t.TaskType,
			Tag:          t.Tag,
			JumpURL:      t.JumpURL,
			Locked:       t.Locked,
			Target:       tgt,
			Current:      cur,
			AcceptStatus: t.AcceptStatus,
			Status:       t.Status,
			Claimable:    !claimed && tgt > 0 && cur >= tgt,
			Claimed:      claimed,
		})
	}
	return out, nil
}

// AcceptTasks 接受任务（幂等：已 accepted 时上游返回成功或业务提示，均不视为致命错误）。
func (c *Client) AcceptTasks(a *auth.Auth, taskCodes []string) error {
	if len(taskCodes) == 0 {
		return nil
	}
	_, err := c.growthJSON(a, http.MethodPost, tasksAcceptPath,
		map[string]any{"task_codes": taskCodes})
	return err
}

// AcceptTasksMP 接受小程序限定任务（缺 mp 头实测返回 task not found）。
func (c *Client) AcceptTasksMP(a *auth.Auth, taskCodes []string) error {
	if len(taskCodes) == 0 {
		return nil
	}
	_, err := c.growthJSONMP(a, http.MethodPost, tasksAcceptPath,
		map[string]any{"task_codes": taskCodes})
	return err
}

// ClaimResult 一次领奖的结果。
type ClaimResult struct {
	Credit         int64
	Energy         int64
	AlreadyClaimed bool
}

// ClaimReward 领取单个任务奖励，返回本次到账的 credit / energy。
//
// 端点：POST {webBase}/activity/growth/tasks/<task_code>/claim
// （任务码在**路径**里，无 body；见文件头「域划分」）
//
// 幂等：已领取过时 already_claimed=true，不算错误，返回零值奖励。
func (c *Client) ClaimReward(a *auth.Auth, taskCode string) (ClaimResult, error) {
	if strings.TrimSpace(taskCode) == "" {
		return ClaimResult{}, fmt.Errorf("claim: empty task code")
	}
	data, err := c.growthJSONWeb(a, http.MethodPost,
		"/activity/growth/tasks/"+url.PathEscape(taskCode)+"/claim", nil)
	if err != nil {
		return ClaimResult{}, err
	}
	return parseClaimReward(data)
}

// ClaimRewardMP 领取小程序限定任务奖励：先走 chatBase 的 mp 口径，
// 遇 400 降级到 Web 域领奖端点。
func (c *Client) ClaimRewardMP(a *auth.Auth, taskCode string) (ClaimResult, error) {
	if strings.TrimSpace(taskCode) == "" {
		return ClaimResult{}, fmt.Errorf("claim: empty task code")
	}
	data, err := c.growthJSONMP(a, http.MethodPost,
		"/activity/growth/tasks/"+url.PathEscape(taskCode)+"/claim", nil)
	if err != nil {
		// chat 域对该路径 400（部分任务/租户形态）→ Web 域降级
		var ue *Error
		if asError(err, &ue) && ue.Status == http.StatusBadRequest {
			return c.ClaimReward(a, taskCode)
		}
		return ClaimResult{}, err
	}
	return parseClaimReward(data)
}

// asError errors.As 的薄封装，避免调用方直接依赖 errors 包。
func asError(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// parseClaimReward 解析领奖响应 data：{"already_claimed","credit","energy"}。
func parseClaimReward(data json.RawMessage) (ClaimResult, error) {
	var resp struct {
		AlreadyClaimed bool  `json:"already_claimed"`
		Credit         int64 `json:"credit"`
		Energy         int64 `json:"energy"`
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &resp); err != nil {
			return ClaimResult{}, err
		}
	}
	return ClaimResult{
		Credit:         resp.Credit,
		Energy:         resp.Energy,
		AlreadyClaimed: resp.AlreadyClaimed,
	}, nil
}

// ---------------------------------------------------------------------------
// 连登（streak）与抽奖
// ---------------------------------------------------------------------------

// StreakTier 一个连登档位。
type StreakTier struct {
	Tier    string `json:"tier"` // "7d" / "14d" / "28d"
	Days    int    `json:"days"`
	Credit  int    `json:"credit"`
	Energy  int    `json:"energy"`
	Cards   int    `json:"cards"`
	Chances int    `json:"chances"`
}

// StreakFull 连登完整状态（GET /activity/growth/streak）。
type StreakFull struct {
	Days              int          `json:"days"`                // 当前连续登录天数
	MonthTotalDays    int          `json:"month_total_days"`    // 本月累计
	NextTier          string       `json:"next_tier"`           //
	NextTierRemaining int          `json:"next_tier_remaining"` //
	Tier7dStatus      string       `json:"tier_7d_status"`      // 可兑换状态
	Tier14dStatus     string       `json:"tier_14d_status"`     //
	Tier28dStatus     string       `json:"tier_28d_status"`     //
	RemainingDays     int          `json:"remaining_days"`      //
	MakeupCardBalance int          `json:"makeup_card_balance"` // 补签卡余额
	MakeupCardMax     int          `json:"makeup_card_max"`     //
	Tiers             []StreakTier `json:"tiers"`               // 各档位奖励明细
}

// GrowthStreakFull 拉取连登完整状态。
//
// 响应形状：上游把 streak / makeup_cards / redemption_status 分组嵌套，
// 这里做扁平化，便于调用方使用。分组字段缺失时对应值保持零值。
func (c *Client) GrowthStreakFull(a *auth.Auth) (*StreakFull, error) {
	data, err := c.growthJSON(a, http.MethodGet, streakPath, nil)
	if err != nil {
		return nil, err
	}
	// 先按分组形态解析（实测形状）
	var grouped struct {
		Streak struct {
			Days              int    `json:"days"`
			MonthTotalDays    int    `json:"month_total_days"`
			NextTier          string `json:"next_tier"`
			NextTierRemaining int    `json:"next_tier_remaining"`
		} `json:"streak"`
		MakeupCards struct {
			Balance int `json:"balance"`
			Max     int `json:"max"`
		} `json:"makeup_cards"`
		RedemptionStatus struct {
			Tier7dStatus  string       `json:"tier_7d_status"`
			Tier14dStatus string       `json:"tier_14d_status"`
			Tier28dStatus string       `json:"tier_28d_status"`
			RemainingDays int          `json:"remaining_days"`
			Tiers         []StreakTier `json:"tiers"`
		} `json:"redemption_status"`
	}
	if err := json.Unmarshal(data, &grouped); err != nil {
		return nil, err
	}
	out := &StreakFull{
		Days:              grouped.Streak.Days,
		MonthTotalDays:    grouped.Streak.MonthTotalDays,
		NextTier:          grouped.Streak.NextTier,
		NextTierRemaining: grouped.Streak.NextTierRemaining,
		MakeupCardBalance: grouped.MakeupCards.Balance,
		MakeupCardMax:     grouped.MakeupCards.Max,
		Tier7dStatus:      grouped.RedemptionStatus.Tier7dStatus,
		Tier14dStatus:     grouped.RedemptionStatus.Tier14dStatus,
		Tier28dStatus:     grouped.RedemptionStatus.Tier28dStatus,
		RemainingDays:     grouped.RedemptionStatus.RemainingDays,
		Tiers:             grouped.RedemptionStatus.Tiers,
	}
	// 若分组形态没解出 streak.days，尝试扁平形态（兼容字段演进）
	if out.Days == 0 && out.Tiers == nil {
		_ = json.Unmarshal(data, out)
	}
	return out, nil
}

// GrowthRedeemTier 兑换连登档位（tier: "7d" | "14d" | "28d"）。
//
// 未解锁返回 403「连续登录天数不足」；调用方应按档位状态跳过而非重试。
func (c *Client) GrowthRedeemTier(a *auth.Auth, tier string) error {
	_, err := c.growthJSON(a, http.MethodPost, streakRedeemPath,
		map[string]any{"tier": tier, "client_token": clientToken()})
	return err
}

// LotteryChances 当前抽奖次数（GET /activity/growth/lottery/summary）。
// 第二个返回值为活动模块是否启用；未启用时次数无意义。
func (c *Client) LotteryChances(a *auth.Auth) (int, bool, error) {
	data, err := c.growthJSON(a, http.MethodGet, lotterySummaryPath, nil)
	if err != nil {
		return 0, false, err
	}
	var resp struct {
		Chances int `json:"chances"`
		Module  struct {
			Enabled bool `json:"enabled"`
		} `json:"module"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, false, err
	}
	return resp.Chances, resp.Module.Enabled, nil
}

// LotteryDraw 抽奖一次，返回原始奖品载荷（形状随活动期变化，透传给调用方）。
func (c *Client) LotteryDraw(a *auth.Auth) (json.RawMessage, error) {
	return c.growthJSON(a, http.MethodPost, lotteryDrawPath,
		map[string]any{"client_token": clientToken()})
}

// ---------------------------------------------------------------------------
// 猫猫旅行（buddy travel）
// ---------------------------------------------------------------------------

// TravelState 猫猫旅行状态。
type TravelState struct {
	State             string `json:"state"`               // idle / traveling / arrived
	DailyLimitReached bool   `json:"daily_limit_reached"` // 今日已派出（自然日 00:00 CST 重置）
	RecordID          int64  `json:"record_id"`           // 在途/到站记录 id，claim 必带
	RewardCredit      int64  `json:"reward_credit"`       // 到站可领奖励
}

// TravelStatus 查询猫猫旅行状态。
func (c *Client) TravelStatus(a *auth.Auth) (*TravelState, error) {
	data, err := c.growthJSON(a, http.MethodGet, travelStatusPath, nil)
	if err != nil {
		return nil, err
	}
	var st TravelState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// TravelDepart 派出猫旅行。locationID 实测 1~4（收益/时长区间相同）。
func (c *Client) TravelDepart(a *auth.Auth, locationID int) error {
	if locationID < 1 || locationID > 4 {
		locationID = 1
	}
	_, err := c.growthJSON(a, http.MethodPost, travelDepartPath,
		map[string]any{"location_id": locationID})
	return err
}

// TravelClaim 领取到站奖励，返回 reward_credit。
// 奖励字段缺失不视为失败（返回 0 由调用方记日志）。
func (c *Client) TravelClaim(a *auth.Auth, recordID int64) (int64, error) {
	data, err := c.growthJSON(a, http.MethodPost, travelClaimPath,
		map[string]any{"record_id": recordID})
	if err != nil {
		return 0, err
	}
	var resp struct {
		RewardCredit int64 `json:"reward_credit"`
	}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &resp)
	}
	return resp.RewardCredit, nil
}

// ---------------------------------------------------------------------------
// Buddy（猫档案：领养 / 协议）
// ---------------------------------------------------------------------------

// Buddy 当前猫档案；nil 表示无猫。
type Buddy struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// BuddyInfo 查询当前猫档案；返回 (nil, nil) 表示无猫。
func (c *Client) BuddyInfo(a *auth.Auth) (*Buddy, error) {
	data, err := c.growthJSON(a, http.MethodGet, buddyInfoPath, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Buddy json.RawMessage `json:"buddy"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	// null / 缺字段 / 空对象都按无猫处理
	trimmed := strings.TrimSpace(string(resp.Buddy))
	if trimmed == "" || trimmed == "null" || trimmed == "{}" {
		return nil, nil
	}
	var b Buddy
	if err := json.Unmarshal(resp.Buddy, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// BuddyAgreement 同意领养协议（幂等，重复调用无副作用）。
func (c *Client) BuddyAgreement(a *auth.Auth) error {
	_, err := c.growthJSON(a, http.MethodPost, buddyAgreePath,
		map[string]any{"agree": true})
	return err
}

// BuddyFirstAdopt 领养第一只猫。无猫且已过对话门槛时送 300 分。
//
// 门槛未达标返回 400（见 IsBuddyTaskIncomplete）—— 属**预期行为**，
// 调用方应静默跳过，当日不要重试（避免对上游重试轰炸）。
func (c *Client) BuddyFirstAdopt(a *auth.Auth) error {
	_, err := c.growthJSON(a, http.MethodPost, buddyFirstPath, map[string]any{})
	return err
}

// GrowthStreakDays 查询连登天数（只读 oracle）。
//
// 用途：活跃上报的自检信号 —— 上报返回 200 但 days 仍为 0，
// 说明上游**静默丢弃**了事件（上报未真正生效）。
func (c *Client) GrowthStreakDays(a *auth.Auth) (int, error) {
	st, err := c.GrowthStreakFull(a)
	if err != nil {
		return 0, err
	}
	return st.Days, nil
}

// IsBuddyTaskIncomplete 判定「领养门槛未达标」：HTTP 400 + first_buddy 关键词。
func IsBuddyTaskIncomplete(err error) bool {
	if err == nil {
		return false
	}
	var ue *Error
	if !asError(err, &ue) || ue.Status != http.StatusBadRequest {
		return false
	}
	return strings.Contains(strings.ToLower(ue.Msg), buddyTaskIncompleteMarker)
}
