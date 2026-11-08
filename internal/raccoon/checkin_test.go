package raccoon

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"omnigate/internal/auth"
	"omnigate/internal/provider"
)

// cstTime 构造 +08:00 的时刻（测试用，避免依赖运行机器时区）。
func cstTime(y int, mo time.Month, d, h, mi, s int) time.Time {
	return time.Date(y, mo, d, h, mi, s, 0, cst)
}

// billJSON 拼一条流水（只关心业务字段）。
func billJSON(bizType, points, createdAt, grantAt, expireAt string) pointsBill {
	n, err := strconv.ParseInt(points, 10, 64)
	if err != nil {
		panic(err)
	}
	return pointsBill{BizType: bizType, Points: n, CreatedAt: createdAt, GrantAt: grantAt, ExpireAt: expireAt}
}

// cstMidnight 取 t 所在 CST 自然日的零点。
//
// **不能用 t.Truncate(24*time.Hour)**：Truncate 按「纪元起的绝对纳秒」对齐，
// 24h 边界落在 UTC 零点 = CST 08:00 —— 于是 CST 00:00–08:00 之间跑测试时，
// 它给出的是**昨天** 08:00，pickTodayDailyGrant 按 GrantAt 判日就会判成今日无发放
// （2026-11-04 00:22 实测复现，R92 修正）。
func cstMidnight(t time.Time) time.Time {
	l := t.In(cst)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, cst)
}

// TestPickTodayDailyGrant 只认「今天、daily_grant、正数」，补发/昨天/清零行都不算。
// R98 起它只作交叉核对，但日期判据仍是「不把昨天的补发当今天」的闸门。
func TestPickTodayDailyGrant(t *testing.T) {
	now := cstTime(2026, 10, 28, 15, 30, 0)
	items := []pointsBill{
		billJSON("daily_expire", "-300", "2026-10-28T00:00:00+08:00", "2026-10-28T00:00:00+08:00", "2026-10-28T23:59:59+08:00"),
		billJSON("daily_grant", "300", "2026-10-28T00:00:00+08:00", "2026-10-27T00:00:00+08:00", "2026-10-27T23:59:59+08:00"),
	}
	if _, ok := pickTodayDailyGrant(items, now); ok {
		t.Fatalf("昨天发放的流水不得算作今日签到")
	}

	items = append(items, billJSON("daily_grant", "300", "2026-10-28T14:57:44+08:00", "2026-10-28T00:00:00+08:00", "2026-11-03T23:59:59+08:00"))
	row, ok := pickTodayDailyGrant(items, now)
	if !ok {
		t.Fatalf("今日 daily_grant 应当命中")
	}
	if row.Points != 300 || row.ExpireAt != "2026-11-03T23:59:59+08:00" {
		t.Fatalf("取到的流水不对: %+v", row)
	}

	items = append(items, billJSON("daily_grant", "500", "2026-10-28T15:00:00+08:00", "2026-10-28T00:00:00+08:00", "2026-11-03T23:59:59+08:00"))
	if row, ok = pickTodayDailyGrant(items, now); !ok || row.Points != 500 {
		t.Fatalf("同日多条应取最大点数，得到 %+v ok=%v", row, ok)
	}
}

// TestPickTodayDailyGrantCrossZone 流水时间戳自带 +08:00，不能因机器时区不同而错判今天。
func TestPickTodayDailyGrantCrossZone(t *testing.T) {
	utcNow := time.Date(2026, 10, 27, 17, 30, 0, 0, time.UTC)
	items := []pointsBill{billJSON("daily_grant", "300", "2026-10-28T00:10:00+08:00", "2026-10-28T00:00:00+08:00", "2026-11-03T23:59:59+08:00")}
	if _, ok := pickTodayDailyGrant(items, utcNow); !ok {
		t.Fatalf("UTC 时刻应按 +08:00 归属到 10-28，判定为今日")
	}
}

// ---------------------------------------------------------------------------
// R98 假上游：setting_info（签到触发）+ bills（交叉核对）+ balance（不该再被打到）
// ---------------------------------------------------------------------------

type popupOut struct {
	Source      string `json:"source"`
	DisplayName string `json:"display_name"`
	Points      int64  `json:"points"`
	ExpireAt    string `json:"expire_at"`
}

type checkinServer struct {
	mu sync.Mutex

	// setting_info 响应
	settingCode int    // HTTP 状态码（0 视为 200）
	settingBody string // 原始 body 覆盖（设置后忽略 popups）
	popups      []GrantPopup
	remain      int64

	// bills 响应（交叉核对）
	bills     []pointsBill
	billsCode int
	billsBody string // 非空时返回 500 + 该 body

	// 计数与最近一次 setting_info 的请求头
	settingHits int
	billsHits   int
	balanceHits int
	settingHdr  http.Header
}

func (cs *checkinServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.mu.Lock()
		defer cs.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, EpOfficeSettingInfo):
			cs.settingHits++
			cs.settingHdr = r.Header.Clone()
			if cs.settingBody != "" {
				code := cs.settingCode
				if code == 0 {
					code = http.StatusOK
				}
				w.WriteHeader(code)
				_, _ = w.Write([]byte(cs.settingBody))
				return
			}
			if cs.settingCode != 0 && cs.settingCode != http.StatusOK {
				w.WriteHeader(cs.settingCode)
				_, _ = w.Write([]byte(`{"code":200001,"message":"authorization_empty_error"}`))
				return
			}
			out := struct {
				Code int `json:"code"`
				Data struct {
					AvailablePoints  int64      `json:"available_points"`
					PointGrantPopups []popupOut `json:"point_grant_popups"`
				} `json:"data"`
			}{Code: 0}
			out.Data.AvailablePoints = cs.remain
			for _, p := range cs.popups {
				out.Data.PointGrantPopups = append(out.Data.PointGrantPopups, popupOut{
					Source: p.Source, DisplayName: p.DisplayName, Points: p.Points, ExpireAt: p.ExpireAt,
				})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(r.URL.Path, EpPointsBalance):
			cs.balanceHits++
			_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"available_points":6556,"daily_points":600}}`))
		case strings.HasSuffix(r.URL.Path, EpPointsBillsV2):
			cs.billsHits++
			if cs.billsBody != "" {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(cs.billsBody))
				return
			}
			out := pointsBillsResp{Data: pointsBillsData{Items: cs.bills}}
			if cs.billsCode != 0 {
				out.Code = cs.billsCode
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
		default:
			t.Errorf("未预期的上游路径: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (cs *checkinServer) snapshot() (setting, bills, balance int, hdr http.Header) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.settingHits, cs.billsHits, cs.balanceHits, cs.settingHdr
}

// newCheckinClient 构造指向假上游的客户端（SiteBase 注入，生产不设）。
func newCheckinClient(srv *httptest.Server) *Client {
	c := NewWithTimeout(5 * time.Second)
	c.SiteBase = srv.URL
	return c
}

func fakeRaccoonAuth() *auth.Auth { return &auth.Auth{AccessToken: "at-test"} }

// todayGrant 造一条「今天的 daily_grant」流水。
func todayGrant() pointsBill {
	now := time.Now().In(cst)
	return billJSON("daily_grant", "300",
		now.Add(-3*time.Hour).Format(time.RFC3339),
		cstMidnight(now).Format(time.RFC3339),
		now.AddDate(0, 0, 6).Format(time.RFC3339))
}

// TestR98SettingInfoUsesOfficialHeaders 锁住 R98 的头对齐：setting_info 必须走
// ApplyHeaders（官方指纹 + 按账号分桶的会话 ID），不能退回 do() 的精简三头。
// 变异点：doOfficial 改回 c.do ⇒ UA 变 Go-http-client、Accept 被手写 ⇒ 本测试红。
func TestR98SettingInfoUsesOfficialHeaders(t *testing.T) {
	cs := &checkinServer{popups: []GrantPopup{{Source: "daily", DisplayName: "每日积分发放", Points: 300, ExpireAt: "2026-11-14T23:59:59+08:00"}}, remain: 6453}
	c := newCheckinClient(cs.start(t))
	if _, _, err := c.FetchSettingInfo(fakeRaccoonAuth()); err != nil {
		t.Fatalf("FetchSettingInfo 不应报错: %v", err)
	}
	_, _, _, hdr := cs.snapshot()
	if hdr == nil {
		t.Fatalf("未捕获到 setting_info 请求头")
	}
	if got := hdr.Get("User-Agent"); got != OfficialUA {
		t.Fatalf("User-Agent=%q，应为官方 %q（裸奔指纹会让签到被上游拒）", got, OfficialUA)
	}
	if got := hdr.Get("X-Stainless-Lang"); got != "python" {
		t.Fatalf("X-Stainless-Lang=%q，官方 SDK 头缺失", got)
	}
	if got := hdr.Get("X-RACCOON-Session-ID"); got == "" {
		t.Fatalf("X-RACCOON-Session-ID 缺失（web 端点按账号分桶）")
	}
	if got := hdr.Get("X-RACCOON-Turn-ID"); got == "" {
		t.Fatalf("X-RACCOON-Turn-ID 缺失")
	}
	if got := hdr.Get("Authorization"); got != "Bearer at-test" {
		t.Fatalf("Authorization=%q", got)
	}
	if got := hdr.Get("Accept"); got != "" {
		t.Fatalf("doOfficial 不应手写 Accept=%q（R91 契约）", got)
	}
}

// TestR98ClaimedFromPopups 核心闸门：setting_info 的 point_grant_popups 非空 ⇒
// 本次真的领到了，且**不再需要查 bills**（bills 只读，不触发发放）。
// 变异点：`if granted > 0` 改成永假 ⇒ 本测试红（签到又回到空转）。
func TestR98ClaimedFromPopups(t *testing.T) {
	cs := &checkinServer{
		popups: []GrantPopup{{Source: "daily", DisplayName: "每日积分发放", Points: 300, ExpireAt: "2026-11-14T23:59:59+08:00"}},
		remain: 6453,
	}
	c := newCheckinClient(cs.start(t))
	rep, err := c.DailyCheckinReport(fakeRaccoonAuth())
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if rep.Status != provider.CheckinClaimed {
		t.Fatalf("状态应为 claimed，得到 %q（msg=%q）", rep.Status, rep.Msg)
	}
	if rep.Status.Retryable() {
		t.Fatalf("claimed 不得可重试")
	}
	if rep.Amount != 300 {
		t.Fatalf("发放额应为 300，得到 %d", rep.Amount)
	}
	if !strings.Contains(rep.Msg, "签到成功") || !strings.Contains(rep.Msg, "300") {
		t.Fatalf("msg 应含「签到成功」与金额，得到 %q", rep.Msg)
	}
	setting, bills, balance, _ := cs.snapshot()
	if setting != 1 {
		t.Fatalf("必须打且只打一次 setting_info（触发动作），实际 %d 次", setting)
	}
	if bills != 0 {
		t.Fatalf("popups 已判 claimed 就不该再查 bills，实际 %d 次", bills)
	}
	if balance != 0 {
		t.Fatalf("balance 是 R61 的错误机制，签到不应再打，实际 %d 次", balance)
	}
}

// TestR98AlreadyWhenPopupsEmptyButBillsHaveGrant 幂等场景：今天已领过 ⇒ popups 空，
// 用流水交叉核对后判 already（不可重试），且 bills 确实被查了。
func TestR98AlreadyWhenPopupsEmptyButBillsHaveGrant(t *testing.T) {
	cs := &checkinServer{bills: []pointsBill{todayGrant()}, remain: 6453}
	c := newCheckinClient(cs.start(t))
	rep, err := c.DailyCheckinReport(fakeRaccoonAuth())
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if rep.Status != provider.CheckinAlready {
		t.Fatalf("状态应为 already_claimed，得到 %q（msg=%q）", rep.Status, rep.Msg)
	}
	if rep.Status.Retryable() {
		t.Fatalf("already_claimed 不得可重试")
	}
	setting, bills, _, _ := cs.snapshot()
	if setting != 1 || bills != 1 {
		t.Fatalf("应各打一次 setting_info 与 bills，实际 setting=%d bills=%d", setting, bills)
	}
}

// TestR98NoCampaignWhenNoGrantAnywhere 反静默空转闸门：setting_info 成功但 popups 空、
// 流水也没有今天的 daily_grant ⇒ 必须判 no_campaign（可重试），不能谎报「已领取」。
// 这正是 2026-10-29 起 20 次签到全部空转的形态，R98 必须把它暴露出来而不是盖住。
// 变异点：末尾 CheckinNoCampaign 改成 CheckinAlready ⇒ 本测试红。
func TestR98NoCampaignWhenNoGrantAnywhere(t *testing.T) {
	yesterday := time.Now().In(cst).AddDate(0, 0, -1)
	cs := &checkinServer{
		bills: []pointsBill{billJSON("daily_grant", "300",
			yesterday.Format(time.RFC3339), yesterday.Format(time.RFC3339), yesterday.Format(time.RFC3339))},
		remain: 6453,
	}
	c := newCheckinClient(cs.start(t))
	rep, err := c.DailyCheckinReport(fakeRaccoonAuth())
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if rep.Status != provider.CheckinNoCampaign {
		t.Fatalf("状态应为 no_campaign，得到 %q（msg=%q）", rep.Status, rep.Msg)
	}
	if !rep.Status.Retryable() {
		t.Fatalf("no_campaign 必须可重试（否则又变成天天静默空转）")
	}
	if !strings.Contains(rep.Msg, "未发放") {
		t.Fatalf("msg 应说明今日未发放，得到 %q", rep.Msg)
	}
}

// TestR98SessionDeadIsTyped 401 必须回 no_token，且错误是 *provider.Error{ErrSessionDead}，
// 否则调度器不会「刷新 token 后重试」（scheduler.checkinOne）。
func TestR98SessionDeadIsTyped(t *testing.T) {
	cs := &checkinServer{settingCode: http.StatusUnauthorized}
	c := newCheckinClient(cs.start(t))
	rep, err := c.DailyCheckinReport(fakeRaccoonAuth())
	if err == nil {
		t.Fatalf("401 应当返回错误")
	}
	if rep.Status != provider.CheckinNoToken {
		t.Fatalf("401 状态应为 no_token，得到 %q", rep.Status)
	}
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Kind != provider.ErrSessionDead {
		t.Fatalf("错误应为 ErrSessionDead，得到 %T %v", err, err)
	}
}

// TestR98SettingFailureIsError setting_info 本身 500 ⇒ CheckinError（可重试）。
func TestR98SettingFailureIsError(t *testing.T) {
	cs := &checkinServer{settingBody: `{"code":500,"message":"internal"}`, settingCode: http.StatusInternalServerError}
	c := newCheckinClient(cs.start(t))
	rep, err := c.DailyCheckinReport(fakeRaccoonAuth())
	if err == nil {
		t.Fatalf("setting_info 500 应当返回错误")
	}
	if rep.Status != provider.CheckinError {
		t.Fatalf("状态应为 error，得到 %q", rep.Status)
	}
}

// TestR98BillsFailureIsConservativeAlready 交叉核对失败不能把「setting_info 已成功」
// 改成错误，否则调度器会拿一个成功动作反复重试。
func TestR98BillsFailureIsConservativeAlready(t *testing.T) {
	cs := &checkinServer{billsBody: `{"code":100002,"message":"params_invalid_error"}`, remain: 6453}
	c := newCheckinClient(cs.start(t))
	rep, err := c.DailyCheckinReport(fakeRaccoonAuth())
	if err != nil {
		t.Fatalf("流水核对失败不应让签到整体失败: %v", err)
	}
	if rep.Status != provider.CheckinAlready {
		t.Fatalf("状态应为 already_claimed，得到 %q", rep.Status)
	}
	if !strings.Contains(rep.Msg, "核对失败") {
		t.Fatalf("msg 应标注未能核对，得到 %q", rep.Msg)
	}
}

// TestR98ZeroPointPopupsIgnored popups 里 points<=0 的条目不得被算成领取。
func TestR98ZeroPointPopupsIgnored(t *testing.T) {
	cs := &checkinServer{
		popups: []GrantPopup{{Source: "info", DisplayName: "提示", Points: 0}},
		bills:  []pointsBill{todayGrant()},
		remain: 6453,
	}
	c := newCheckinClient(cs.start(t))
	rep, err := c.DailyCheckinReport(fakeRaccoonAuth())
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if rep.Status != provider.CheckinAlready {
		t.Fatalf("0 分 popups 不应判 claimed，得到 %q", rep.Status)
	}
	if rep.Amount != 0 {
		t.Fatalf("发放额应为 0，得到 %d", rep.Amount)
	}
}

// TestR98DailyCheckinDelegatesReport DailyCheckin 与 Report 口径一致（兼容旧接口）：
// already/claimed 返回 nil；no_campaign 必须返回错误，供未实现 Reporter 的调用方判定。
// 变异点：`if rep.Status.Retryable()` 改成永假 ⇒ 本测试红。
func TestR98DailyCheckinDelegatesReport(t *testing.T) {
	cs := &checkinServer{bills: []pointsBill{todayGrant()}, remain: 6453}
	c := newCheckinClient(cs.start(t))
	if err := c.DailyCheckin(fakeRaccoonAuth()); err != nil {
		t.Fatalf("已领取时 DailyCheckin 应返回 nil: %v", err)
	}

	cs2 := &checkinServer{remain: 6453} // popups 空、bills 空 ⇒ no_campaign
	c2 := newCheckinClient(cs2.start(t))
	if err := c2.DailyCheckin(fakeRaccoonAuth()); err == nil {
		t.Fatalf("未发放时 DailyCheckin 应返回错误（否则调用方误以为签到成功）")
	} else if !strings.Contains(err.Error(), "每日积分") && !strings.Contains(err.Error(), "未发放") {
		t.Fatalf("错误文案应说明每日积分未发放，得到 %v", err)
	}
}
