package raccoon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

// TestPickTodayDailyGrant 只认「今天、daily_grant、正数」，补发/昨天/清零行都不算。
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

	// 同日多条（正常发放 + 补发）取点数最大的一条
	items = append(items, billJSON("daily_grant", "500", "2026-10-28T15:00:00+08:00", "2026-10-28T00:00:00+08:00", "2026-11-03T23:59:59+08:00"))
	if row, ok = pickTodayDailyGrant(items, now); !ok || row.Points != 500 {
		t.Fatalf("同日多条应取最大点数，得到 %+v ok=%v", row, ok)
	}
}

// TestPickTodayDailyGrantCrossZone 流水时间戳自带 +08:00，不能因机器时区不同而错判今天。
func TestPickTodayDailyGrantCrossZone(t *testing.T) {
	// UTC 视角下 2026-10-27T17:30Z 就是 +08:00 的 10-28 01:30
	utcNow := time.Date(2026, 10, 27, 17, 30, 0, 0, time.UTC)
	items := []pointsBill{billJSON("daily_grant", "300", "2026-10-28T00:10:00+08:00", "2026-10-28T00:00:00+08:00", "2026-11-03T23:59:59+08:00")}
	if _, ok := pickTodayDailyGrant(items, utcNow); !ok {
		t.Fatalf("UTC 时刻应按 +08:00 归属到 10-28，判定为今日")
	}
}

// checkinServer 起一个假上游：按路径返回余额与流水。
type checkinServer struct {
	bills     []pointsBill
	billsCode int
	billsBody string
	balance   int // HTTP 状态码（0 视为 200）
	hits      int
}

func (cs *checkinServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.hits++
		switch {
		case strings.HasSuffix(r.URL.Path, EpPointsBalance):
			code := cs.balance
			if code == 0 {
				code = http.StatusOK
			}
			w.WriteHeader(code)
			if code == http.StatusOK {
				_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"available_points":6556,"daily_points":600}}`))
			} else {
				_, _ = w.Write([]byte(`{"code":200001,"message":"authorization_empty_error"}`))
			}
		case strings.HasSuffix(r.URL.Path, EpPointsBillsV2):
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

// newCheckinClient 构造指向假上游的客户端（SiteBase 注入，生产不设）。
func newCheckinClient(srv *httptest.Server) *Client {
	c := NewWithTimeout(5 * time.Second)
	c.SiteBase = srv.URL
	return c
}

func fakeRaccoonAuth() *auth.Auth { return &auth.Auth{AccessToken: "at-test"} }

// TestDailyCheckinReportClaimedWhenFresh 流水落账时间就在本次调用窗口内 → claimed。
func TestDailyCheckinReportClaimedWhenFresh(t *testing.T) {
	now := time.Now().In(cst)
	cs := &checkinServer{bills: []pointsBill{billJSON("daily_grant", "300",
		now.Format(time.RFC3339), now.Truncate(24*time.Hour).Format(time.RFC3339), "2026-11-03T23:59:59+08:00")}}
	c := newCheckinClient(cs.start(t))
	rep, err := c.DailyCheckinReport(fakeRaccoonAuth())
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if rep.Status != provider.CheckinClaimed {
		t.Fatalf("状态应为 claimed，得到 %q（msg=%q）", rep.Status, rep.Msg)
	}
	if rep.Amount != 300 {
		t.Fatalf("发放额应为 300，得到 %d", rep.Amount)
	}
	if !strings.Contains(rep.Msg, "300") || !strings.Contains(rep.Msg, "到期") {
		t.Fatalf("msg 应含金额与到期日，得到 %q", rep.Msg)
	}
	if cs.hits != 2 {
		t.Fatalf("应当只摸余额+流水各一次，实际 %d 次", cs.hits)
	}
}

// TestDailyCheckinReportAlreadyWhenEarlier 当日流水早就落账 → already_claimed（不可重试）。
func TestDailyCheckinReportAlreadyWhenEarlier(t *testing.T) {
	now := time.Now().In(cst)
	early := now.Add(-6 * time.Hour).Format(time.RFC3339)
	cs := &checkinServer{bills: []pointsBill{billJSON("daily_grant", "300", early,
		now.Truncate(24*time.Hour).Format(time.RFC3339), "2026-11-03T23:59:59+08:00")}}
	c := newCheckinClient(cs.start(t))
	rep, err := c.DailyCheckinReport(fakeRaccoonAuth())
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if rep.Status != provider.CheckinAlready {
		t.Fatalf("状态应为 already_claimed，得到 %q", rep.Status)
	}
	if rep.Status.Retryable() {
		t.Fatalf("already_claimed 不得可重试")
	}
	// 面板只有 Msg 一个通道，两种「今天已有」必须文案可区分。
	if strings.Contains(rep.Msg, "+300") || !strings.Contains(rep.Msg, "已发放") {
		t.Fatalf("非本次触发时不得显示金额增加，得到 %q", rep.Msg)
	}
}

// TestDailyCheckinReportNoCampaignWhenMissing 流水里没有今天的 daily_grant → no_campaign（可重试）。
func TestDailyCheckinReportNoCampaignWhenMissing(t *testing.T) {
	yesterday := time.Now().In(cst).AddDate(0, 0, -1)
	cs := &checkinServer{bills: []pointsBill{billJSON("daily_grant", "300",
		yesterday.Format(time.RFC3339), yesterday.Format(time.RFC3339), "2026-10-27T23:59:59+08:00")}}
	c := newCheckinClient(cs.start(t))
	rep, err := c.DailyCheckinReport(fakeRaccoonAuth())
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if rep.Status != provider.CheckinNoCampaign {
		t.Fatalf("状态应为 no_campaign，得到 %q", rep.Status)
	}
	if !rep.Status.Retryable() {
		t.Fatalf("no_campaign 必须可重试（上游懒发放可能延后）")
	}
}

// TestDailyCheckinReportSessionDeadIsTyped 401 必须回 provider.ErrSessionDead，
// 否则调度器不会刷新 token 后重试（见 scheduler.checkinOne）。
func TestDailyCheckinReportSessionDeadIsTyped(t *testing.T) {
	cs := &checkinServer{balance: http.StatusUnauthorized}
	c := newCheckinClient(cs.start(t))
	rep, err := c.DailyCheckinReport(fakeRaccoonAuth())
	if err == nil {
		t.Fatalf("401 应当返回错误")
	}
	if rep.Status != provider.CheckinNoToken {
		t.Fatalf("401 状态应为 no_token，得到 %q", rep.Status)
	}
	if Classify(401, "") != provider.ErrSessionDead {
		t.Fatalf("前提失效：Classify(401) 应为 session dead")
	}
}

// TestDailyCheckinReportBillsFailure 流水接口 500 → error 状态且返回错误（可重试）。
func TestDailyCheckinReportBillsFailure(t *testing.T) {
	cs := &checkinServer{billsBody: `{"code":100002,"message":"params_invalid_error"}`, balance: http.StatusOK}
	c := newCheckinClient(cs.start(t))
	rep, err := c.DailyCheckinReport(fakeRaccoonAuth())
	if err == nil {
		t.Fatalf("流水 500 应当返回错误")
	}
	if rep.Status != provider.CheckinError {
		t.Fatalf("状态应为 error，得到 %q", rep.Status)
	}
}

// TestDailyCheckinDelegatesReport DailyCheckin 与 Report 口径一致（兼容旧接口）。
func TestDailyCheckinDelegatesReport(t *testing.T) {
	now := time.Now().In(cst)
	cs := &checkinServer{bills: []pointsBill{billJSON("daily_grant", "300",
		now.Add(-3*time.Hour).Format(time.RFC3339), now.Truncate(24*time.Hour).Format(time.RFC3339), "")}}
	c := newCheckinClient(cs.start(t))
	if err := c.DailyCheckin(fakeRaccoonAuth()); err != nil {
		t.Fatalf("已发放时 DailyCheckin 应返回 nil: %v", err)
	}

	cs2 := &checkinServer{bills: nil}
	c2 := newCheckinClient(cs2.start(t))
	if err := c2.DailyCheckin(fakeRaccoonAuth()); err == nil {
		t.Fatalf("未发放时 DailyCheckin 应返回错误（供未实现 Reporter 的调用方判定）")
	} else if !strings.Contains(err.Error(), "每日积分") {
		t.Fatalf("错误文案应说明每日积分未发放，得到 %v", err)
	}
}
