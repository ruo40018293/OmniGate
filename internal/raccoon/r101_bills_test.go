// r101_bills_test.go 锁住 R101：小浣熊流水查询必须带 type=income。
//
// 为什么必须带：不带筛选时这一页混着「桌面端 LLM 对话消耗」，每笔占一条；服务端只给
// 最近 N 条且 offset 翻不页（实测 offset>0 恒空）。当天发放记录几小时后就被挤出窗口，
// pickTodayDailyGrant 找不到今天的 daily_grant ⇒ 把「今天已领过」误判成「今天根本没发」
// ⇒ 触发无谓重试。收入流水每天 1 笔左右，窗口不会再被消耗冲掉。
package raccoon

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"omnigate/internal/auth"
)

// TestFetchBillsRequestsIncomeOnly 请求必须带 type=income。
func TestFetchBillsRequestsIncomeOnly(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"items":[]}}`))
	}))
	defer srv.Close()

	c := New()
	c.SiteBase = srv.URL
	if _, err := c.fetchBills(&auth.Auth{AccessToken: "dummy"}, 30); err != nil {
		t.Fatalf("fetchBills: %v", err)
	}
	if !strings.Contains(gotQuery, "type=income") {
		t.Fatalf("流水请求必须带 type=income（否则消耗流水把当天发放挤出窗口），实际 query=%q", gotQuery)
	}
	if strings.Contains(gotQuery, "type=all") {
		t.Fatalf("不得再带 type=all，实际 query=%q", gotQuery)
	}
	if !strings.Contains(gotQuery, "paging.limit=30") {
		t.Errorf("应带分页上限，实际 query=%q", gotQuery)
	}
}

// TestFetchBillsDefaultLimit 传 0 或负数时用 billLookback。
func TestFetchBillsDefaultLimit(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"code":0,"data":{"items":[]}}`))
	}))
	defer srv.Close()

	c := New()
	c.SiteBase = srv.URL
	for _, limit := range []int{0, -5} {
		if _, err := c.fetchBills(&auth.Auth{AccessToken: "dummy"}, limit); err != nil {
			t.Fatalf("fetchBills(%d): %v", limit, err)
		}
		want := "paging.limit=" + strconv.Itoa(billLookback)
		if !strings.Contains(gotQuery, want) {
			t.Errorf("limit=%d 应回退到 billLookback，实际 query=%q", limit, gotQuery)
		}
	}
}

// rfcs 把「今天的 hh:mm:ss」拼成上游用的 RFC3339（+08:00）。
func rfcs(day string, hms string) string {
	return day + "T" + hms + "+08:00"
}

// TestPickTodayDailyGrantIgnoresConsumption 消耗流水不得干扰「今天是否已发放」的判定。
func TestPickTodayDailyGrantIgnoresConsumption(t *testing.T) {
	now := time.Now()
	today := now.In(cst).Format("2006-01-02")
	bills := []pointsBill{
		{BizType: "llm_consume", Points: -5, GrantAt: rfcs(today, "10:00:00"), EventName: "对话消耗"},
		{BizType: "llm_consume", Points: -3, GrantAt: rfcs(today, "10:05:00"), EventName: "对话消耗"},
		{BizType: "daily_grant", Points: 300, GrantAt: rfcs(today, "08:00:00"), EventName: "每日赠送"},
	}
	got, ok := pickTodayDailyGrant(bills, now)
	if !ok {
		t.Fatal("今天的 daily_grant 必须被找到（否则会误判成今天没发）")
	}
	if got.Points != 300 {
		t.Errorf("应选中发放那条（300 分），得到 %d", got.Points)
	}
}

// TestPickTodayDailyGrantIgnoresYesterday 昨天的发放不算今天。
func TestPickTodayDailyGrantIgnoresYesterday(t *testing.T) {
	now := time.Now()
	yest := now.In(cst).AddDate(0, 0, -1).Format("2006-01-02")
	bills := []pointsBill{
		{BizType: "daily_grant", Points: 300, GrantAt: rfcs(yest, "08:00:00")},
	}
	if _, ok := pickTodayDailyGrant(bills, now); ok {
		t.Fatal("昨天的发放不得算作今天已领")
	}
}

// TestPickTodayDailyGrantIgnoresZeroPoints 0 分或负分的 daily_grant 不算发放。
func TestPickTodayDailyGrantIgnoresZeroPoints(t *testing.T) {
	now := time.Now()
	today := now.In(cst).Format("2006-01-02")
	for _, pts := range []int64{0, -300} {
		bills := []pointsBill{
			{BizType: "daily_grant", Points: pts, GrantAt: rfcs(today, "08:00:00")},
		}
		if _, ok := pickTodayDailyGrant(bills, now); ok {
			t.Errorf("daily_grant 且 Points=%d 不该算作已发放", pts)
		}
	}
}

// TestPickTodayDailyGrantFallsBackToCreatedAt grant_at 缺失时用 created_at。
func TestPickTodayDailyGrantFallsBackToCreatedAt(t *testing.T) {
	now := time.Now()
	today := now.In(cst).Format("2006-01-02")
	bills := []pointsBill{
		{BizType: "daily_grant", Points: 300, CreatedAt: rfcs(today, "09:30:00")},
	}
	if _, ok := pickTodayDailyGrant(bills, now); !ok {
		t.Fatal("grant_at 缺失时应回退 created_at")
	}
}

// TestPickTodayDailyGrantPicksLargest 同日多条发放取最大那条。
func TestPickTodayDailyGrantPicksLargest(t *testing.T) {
	now := time.Now()
	today := now.In(cst).Format("2006-01-02")
	bills := []pointsBill{
		{BizType: "daily_grant", Points: 100, GrantAt: rfcs(today, "08:00:00")},
		{BizType: "daily_grant", Points: 300, GrantAt: rfcs(today, "09:00:00")},
		{BizType: "daily_grant", Points: 200, GrantAt: rfcs(today, "10:00:00")},
	}
	got, ok := pickTodayDailyGrant(bills, now)
	if !ok || got.Points != 300 {
		t.Fatalf("应取最大的一条（300），得到 %d ok=%v", got.Points, ok)
	}
}
