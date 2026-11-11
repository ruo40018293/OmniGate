package raccoon

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"omnigate/internal/auth"
	"omnigate/internal/provider"
)

// ---------------------------------------------------------------------------
// 每日积分「签到」（R98，2026-11-08 修正 R61 的错误结论）
// ---------------------------------------------------------------------------
//
// R61 的结论是「当天首次触达积分服务即懒发放」，于是签到 = 摸 balance + 核对 bills。
// 这是**错的**：2026-10-29 之后用户不再开网页，调度器每天 09:00/21:00 都去摸
// balance/bills，20 次全部返回「上游今日每日积分尚未发放」；积分以 daily_makeup
// 形式补发且当日 24:00 清零，余额长期不涨（2026-11-08 生产日志取证）。
//
// 上游 internal/raccoon/checkin.go（2026-10-15 就存在，fork 点之前，所以此前
// 基于 commit/diff 的盘点一直没看见）给出了真实机制：
//
//   - **触发点是 GET /api/web/office/v3/setting_info**（工作台设置），其响应
//     `data.point_grant_popups[]` 是「本次调用刚触发的发放」的直接回执。
//   - POST /api/web/llm/v2/chat/completions 只扣分，不入账；
//     GET /api/web/points/v1/balance、/api/web/points/v2/bills **只读，不入账**；
//     GET /api/web/auth/v1/user_info、POST /api/web/auth/v1/iam/confirm 也不入账。
//   - 幂等：当天重复调 setting_info 不重复发放（第二次 popups 为空数组）。
//   - 网页文案：「每日积分每天 00:00 发放，默认当日 24:00 清零，当天登录后
//     有效期延长至 7 天。」
//
// 所以本渠道的「签到」= 主动打一次 setting_info（等价于替用户打开一次网页），
// 再用 popups 判本次是否真的领到。bills 保留为**交叉核对**：popups 为空时用它
// 区分「今天早就领过」与「今天根本没发」（后者必须可重试，否则又变成天天静默空转）。
//
// 出站头走 ApplyHeaders（官方 box-agent 形态）：上游 R148 把 do() 也统一到了这套头，
// 我们 R91 只接了 ChatStream 那一半，R98 在 setting_info 上补齐（见 doOfficial）。

// cst 上游业务时区（所有流水时间戳都是 +08:00，与运行机器时区无关）。
var cst = time.FixedZone("CST", 8*3600)

// billLookback 交叉核对时读的流水条数。
const billLookback = 30

// settingInfoResp /api/web/office/v3/setting_info 的响应。
// 只解析签到关心的两个字段；其余（空间配额/功能开关/ab_tests 等）不建模，
// 上游增删这些字段不应影响签到。
type settingInfoResp struct {
	Code int `json:"code"`
	Data struct {
		AvailablePoints int64 `json:"available_points"`
		// PointGrantPopups 本次调用**刚刚触发**的发放提示列表。
		// 非空 ⇔ 本次触发了新发放；空 ⇔ 今天已经领过（幂等语义的判据）。
		PointGrantPopups []struct {
			Source      string `json:"source"`
			DisplayName string `json:"display_name"`
			Points      int64  `json:"points"`
			ExpireAt    string `json:"expire_at"`
		} `json:"point_grant_popups"`
	} `json:"data"`
}

// GrantPopup 一次发放提示（setting_info 的 point_grant_popups 条目）。
type GrantPopup struct {
	Source      string `json:"source"`
	DisplayName string `json:"display_name"`
	Points      int64  `json:"points"`
	ExpireAt    string `json:"expire_at"`
}

// pointsBill 是 /api/web/points/v2/bills 的单条流水（字段取自实测响应）。
type pointsBill struct {
	BillID    string `json:"bill_id"`
	EventName string `json:"event_name"`
	BizType   string `json:"biz_type"`
	Points    int64  `json:"points"`
	CreatedAt string `json:"created_at"`
	GrantAt   string `json:"grant_at"`
	ExpireAt  string `json:"expire_at"`
}

// pointsBillsData 流水响应的 data 段。
type pointsBillsData struct {
	Items    []pointsBill `json:"items"`
	HasMore  bool         `json:"has_more"`
	NextCurs string       `json:"next_cursor"`
}

// pointsBillsResp 流水响应信封（code=0 成功；100002 为参数错误）。
type pointsBillsResp struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    pointsBillsData `json:"data"`
}

// FetchSettingInfo 拉取工作台设置（副作用：触发当日每日积分落账，幂等）。
//
// 这是本渠道「签到」的**实质动作**——上游没有独立领取端点，发放靠这次访问触发。
// 返回 (可用余额, 本次发放的积分列表)。
func (c *Client) FetchSettingInfo(a *auth.Auth) (int64, []GrantPopup, error) {
	resp, raw, err := c.doOfficial(http.MethodGet, c.siteBase()+EpOfficeSettingInfo, a, nil)
	if err != nil {
		return 0, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, nil, &provider.Error{
			Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: truncate(string(raw), 300),
		}
	}
	var out settingInfoResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, nil, fmt.Errorf("raccoon setting_info 响应解析失败: %w (%s)", err, truncate(string(raw), 200))
	}
	if out.Code != 0 {
		// 业务码非 0 视为失败（与 points 服务同口径：只有 HTTP 状态码能判会话失效）。
		return 0, nil, &provider.Error{
			Kind: provider.ErrClient, Status: resp.StatusCode, Msg: truncate(string(raw), 300),
		}
	}
	popups := make([]GrantPopup, 0, len(out.Data.PointGrantPopups))
	for _, p := range out.Data.PointGrantPopups {
		popups = append(popups, GrantPopup{
			Source: p.Source, DisplayName: p.DisplayName, Points: p.Points, ExpireAt: p.ExpireAt,
		})
	}
	return out.Data.AvailablePoints, popups, nil
}

// DailyCheckinReport 实现 provider.CheckinReporter：打一次 setting_info 触发发放，
// 用 point_grant_popups 判本次是否领到；popups 为空时用流水交叉核对。
//
// 状态映射：
//   - popups 含正数积分                 → CheckinClaimed（本次真正领到，不可重试）
//   - popups 空 + 今日已有 daily_grant  → CheckinAlready（今天早就领过，不可重试）
//   - popups 空 + 今日无 daily_grant    → CheckinNoCampaign（可重试：触发没生效，
//     不能谎报「已领取」，否则就回到 R61 那种天天静默空转）
//   - popups 空 + 流水查询失败          → CheckinAlready（setting_info 本身成功，
//     保守不把成功改成错误，但 msg 标注未能核对）
//   - setting_info 失败                → CheckinNoToken（401/403）或 CheckinError
//
// 日志形状与上游同构（本仓 stats 并不解析 raccoon 行，保留形状只为日后对齐）。
func (c *Client) DailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {
	remain, popups, err := c.FetchSettingInfo(a)
	if err != nil {
		rep := provider.CheckinReport{Status: reportStatusFor(err), Msg: shortText(err.Error())}
		log.Printf("raccoon checkin setting_info uid=%s status=%s claimed=false amount=0 msg=%s err=%v",
			a.UID, rep.Status, rep.Msg, err)
		return rep, err
	}

	var granted int64
	var name string
	var exp string
	for _, p := range popups {
		if p.Points <= 0 {
			continue
		}
		granted += p.Points
		if name == "" && p.DisplayName != "" {
			name = p.DisplayName
		}
		if exp == "" && p.ExpireAt != "" {
			exp = expireDay(p.ExpireAt)
		}
	}
	if granted > 0 {
		msg := fmt.Sprintf("签到成功 +%d", granted)
		if name != "" {
			msg += "（" + name + "）"
		}
		if exp != "" {
			msg += "（" + exp + " 到期）"
		}
		rep := provider.CheckinReport{Status: provider.CheckinClaimed, Msg: msg, Amount: granted}
		log.Printf("raccoon checkin setting_info uid=%s status=%s claimed=true amount=%d msg=%s err=%v remain=%d",
			a.UID, rep.Status, granted, rep.Msg, nil, remain)
		return rep, nil
	}

	// popups 为空：用流水交叉核对，区分「今天已领过」与「今天根本没发」。
	bills, berr := c.fetchBills(a, billLookback)
	if berr != nil {
		rep := provider.CheckinReport{
			Status: provider.CheckinAlready,
			Msg:    "今日已领取（流水核对失败，仅按 setting_info 判定）",
		}
		log.Printf("raccoon checkin setting_info uid=%s status=%s claimed=false amount=0 msg=%s err=%v remain=%d",
			a.UID, rep.Status, rep.Msg, berr, remain)
		return rep, nil
	}
	if row, ok := pickTodayDailyGrant(bills, time.Now()); ok {
		msg := "今日已领取"
		if d := expireDay(row.ExpireAt); d != "" {
			msg += "（" + d + " 到期）"
		}
		rep := provider.CheckinReport{Status: provider.CheckinAlready, Msg: msg}
		log.Printf("raccoon checkin setting_info uid=%s status=%s claimed=false amount=0 msg=%s err=%v remain=%d",
			a.UID, rep.Status, rep.Msg, nil, remain)
		return rep, nil
	}
	rep := provider.CheckinReport{
		Status: provider.CheckinNoCampaign,
		Msg:    "签到已触发但上游今日未发放每日积分（当日窗口内可重试）",
	}
	log.Printf("raccoon checkin setting_info uid=%s status=%s claimed=false amount=0 msg=%s err=%v remain=%d",
		a.UID, rep.Status, rep.Msg, nil, remain)
	return rep, nil
}

// fetchBills 拉最近 limit 条流水（type=all）——R98 起只作交叉核对，不再作触发动作。
//
// 参数口径取自官方前端 points 客户端：`paging.limit=<n>&type=all`；
// 缺 type 会被上游按 all 处理，但显式传更稳妥（type 取非法值会 400/100002）。
func (c *Client) fetchBills(a *auth.Auth, limit int) ([]pointsBill, error) {
	if limit <= 0 {
		limit = billLookback
	}
	// R101：改用 type=income。不带筛选时这一页混着「桌面端 LLM 对话消耗」，每笔占一条；
	// 服务端只给最近 N 条且 offset 翻不页（实测 offset>0 恒空），当天发放记录几小时后就被
	// 挤出窗口 ⇒ pickTodayDailyGrant 找不到今天的 daily_grant，把「今天已领过」误判成
	// 「今天根本没发」，触发无谓的重试。收入流水每天 1 笔左右，窗口不会再被消耗冲掉。
	url := fmt.Sprintf("%s%s?paging.limit=%d&type=income", c.siteBase(), EpPointsBillsV2, limit)
	resp, raw, err := c.do(http.MethodGet, url, a, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &provider.Error{
			Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: truncate(string(raw), 300),
		}
	}
	var out pointsBillsResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("raccoon 流水响应解析失败: %w (%s)", err, truncate(string(raw), 200))
	}
	if out.Code != 0 {
		return nil, fmt.Errorf("raccoon 流水查询失败: code=%d %s", out.Code, truncate(string(raw), 200))
	}
	return out.Data.Items, nil
}

// pickTodayDailyGrant 从流水里挑出「今天发放的每日积分」。
//
// 判据：biz_type=daily_grant 且 grant_at（缺失时回落 created_at）在**今天**（+08:00）。
// 同一天可能有多条（正常发放 + 补发 daily_makeup），取点数最大的一条作为当日额度。
// 抽成纯函数便于单测（不碰网络）。
func pickTodayDailyGrant(items []pointsBill, now time.Time) (pointsBill, bool) {
	today := now.In(cst).Format("2006-01-02")
	var best pointsBill
	found := false
	for _, it := range items {
		if it.BizType != "daily_grant" || it.Points <= 0 {
			continue
		}
		ref := it.GrantAt
		if ref == "" {
			ref = it.CreatedAt
		}
		t, err := time.Parse(time.RFC3339, ref)
		if err != nil {
			continue
		}
		if t.In(cst).Format("2006-01-02") != today {
			continue
		}
		if !found || it.Points > best.Points {
			best, found = it, true
		}
	}
	return best, found
}

// expireDay 把 expire_at 压成「2026-11-03」展示；解析失败返回空串（不编造日期）。
func expireDay(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return ""
	}
	return t.In(cst).Format("2006-01-02")
}

// reportStatusFor 把错误映射成签到状态（决定调度器是否在当日窗口内重试）。
func reportStatusFor(err error) provider.CheckinStatus {
	var pe *provider.Error
	if errors.As(err, &pe) && pe.Kind == provider.ErrSessionDead {
		return provider.CheckinNoToken // 需要刷新/重新登录，可重试
	}
	return provider.CheckinError
}

// shortText 截断错误文案，避免面板/日志被上游长 body 撑爆。
func shortText(s string) string {
	if len(s) <= 120 {
		return s
	}
	return s[:120]
}
