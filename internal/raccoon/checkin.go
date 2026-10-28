package raccoon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"omnigate/internal/auth"
	"omnigate/internal/provider"
)

// ---------------------------------------------------------------------------
// 每日积分「签到」（R61）
// ---------------------------------------------------------------------------
//
// 结论（2026-10-28 真实账号取证，勿凭直觉改）：
//
//  1. 小浣熊**没有**「点击领取」的签到接口。官方前端的 points 客户端只有
//     `getBalance()`（/api/web/points/v1/balance）与 `getBills()`
//     （/api/web/points/v2/bills），没有 sign/checkin/claim 之类的方法；
//     整个前端主包也搜不到「签到」字样。
//  2. 每日积分由服务端**懒发放**：当天首次触达积分服务时补写一条流水
//     `biz_type="daily_grant"`、`event_name="每日积分发放"`、`points=300`、
//     `grant_at` 为当天 00:00、`created_at` 才是真正落账时刻（实测 14:57:44 /
//     16:27:39 / 23:04:58 —— 都等于当天第一次用它的时间，不是定时任务时间）。
//  3. 每日积分**有有效期**（实测约 7 天，`expire_at` 落账日+6 天 23:59:59），
//     到期由 `biz_type="daily_expire"` 清零 ⇒ 漏一天就真少 300，必须每天领。
//
// 所以本渠道的「签到」= 每天主动摸一次积分服务，再核对当日流水是否已发放。
// 两个动作都是 GET，幂等、不消耗额度、不会重复领取（服务端按天去重）。

// cst 上游业务时区（所有流水时间戳都是 +08:00，与运行机器时区无关）。
var cst = time.FixedZone("CST", 8*3600)

// billLookback 读流水条数。日常用量下一天几十条，取 30 足够覆盖到当天的
// daily_grant（它在流水里按 created_at 排序，落账即靠前）。
const billLookback = 30

// claimedWindow 判定「本次领取」的时间窗：流水的 created_at 落在
// [本次调用开始-该窗, 现在] 内，说明是这次调用把当日积分摸出来的。
const claimedWindow = 90 * time.Second

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

// DailyCheckinReport 实现 provider.CheckinReporter：摸一次积分服务 + 核对当日流水。
//
// 状态映射：
//   - 当日有 daily_grant 且落账时间在本次调用窗口内 → CheckinClaimed（本次摸出来的）
//   - 当日有 daily_grant 但落账更早                   → CheckinAlready（今天早就发过了）
//   - 当日没有 daily_grant                             → CheckinNoCampaign（可重试：
//     上游可能还没跑到补发逻辑，窗口内再摸一次往往就有了）
//   - 余额/流水接口失败                                → CheckinNoToken（401/403）或 CheckinError
func (c *Client) DailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {
	start := time.Now()
	// ① 触达积分服务：服务端在此补写当天的 daily_grant（懒发放）。
	if _, _, err := c.UserResourceDetail(a); err != nil {
		return provider.CheckinReport{Status: reportStatusFor(err), Msg: shortText(err.Error())}, err
	}
	// ② 读流水核对：签到是否真的到账，只认 daily_grant 这一行，不看 HTTP 200。
	bills, err := c.fetchBills(a, billLookback)
	if err != nil {
		return provider.CheckinReport{Status: reportStatusFor(err), Msg: "流水查询失败：" + shortText(err.Error())}, err
	}
	row, ok := pickTodayDailyGrant(bills, time.Now())
	if !ok {
		return provider.CheckinReport{
			Status: provider.CheckinNoCampaign,
			Msg:    "上游今日每日积分尚未发放（当日窗口内可重试）",
		}, nil
	}
	// 两种「今天已有」必须能一眼分清，否则用户无法判断是「这次摸出来了」还是
	// 「今天早些时候已经发过」——面板只有 Msg 一个通道，不能都写成 "+300"。
	status := provider.CheckinAlready
	msg := fmt.Sprintf("今日已发放 %d（不是本次触发）", row.Points)
	if created, perr := time.Parse(time.RFC3339, row.CreatedAt); perr == nil && !created.Before(start.Add(-claimedWindow)) {
		status = provider.CheckinClaimed
		msg = fmt.Sprintf("每日积分 +%d", row.Points)
	}
	if exp := expireDay(row.ExpireAt); exp != "" {
		msg += "（" + exp + " 到期）"
	}
	return provider.CheckinReport{Status: status, Msg: msg, Amount: row.Points}, nil
}

// fetchBills 拉最近 limit 条流水（type=all）。
//
// 参数口径取自官方前端 points 客户端：`paging.limit=<n>&type=all`；
// 缺 type 会被上游按 all 处理，但显式传更稳妥（type 取非法值会 400/100002）。
func (c *Client) fetchBills(a *auth.Auth, limit int) ([]pointsBill, error) {
	if limit <= 0 {
		limit = billLookback
	}
	url := fmt.Sprintf("%s%s?paging.limit=%d&type=all", c.siteBase(), EpPointsBillsV2, limit)
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
