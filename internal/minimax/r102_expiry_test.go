package minimax

// r102_expiry_test.go 锁住「积分批次到期时间」这条链路。
//
// 为什么值得单独测：到期时刻只存在于 `credit/details` 的**批次层**
// （credit_type / remaining_amount / expire_at_ms），余额汇总那一层根本没有它。
// 漏读不会报错，只会让 `ResourceItem.ExpireAt` 恒为空 —— 面板不显示有效期、
// 选号也永远排在有到期信息的账号之后，属于「静默降级成更差的结果」。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// ledgerPage 生成一页积分明细响应。details 里的每一项是已经序列化的 JSON 对象。
func ledgerPage(total int, details ...string) string {
	body := `{"base_resp":{"status_code":0,"status_msg":"success"},"total_count":` +
		fmt.Sprintf("%d", total)
	if len(details) > 0 {
		body += `,"details":[`
		for i, d := range details {
			if i > 0 {
				body += ","
			}
			body += d
		}
		body += `]`
	}
	return body + `}`
}

// expiryClient 起一个假上游，按 page_number 返回对应页；返回被请求过的页号。
//
// 注意：余额汇总与账本第 1 页是**同一个端点**（只差 query），所以不带
// page_number 的那次请求按第 1 页的内容回——这正是上游的真实行为。
// seen 只记录**带 page_number** 的请求，否则余额请求会被算成一次账本读取。
func expiryClient(t *testing.T, pages map[int]string) (*Client, *[]int) {
	t.Helper()
	var seen []int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpCreditDetails {
			_, _ = io.WriteString(w, `{"base_resp":{"status_code":0},"total_count":0}`)
			return
		}
		raw := r.URL.Query().Get("page_number")
		page := 1
		if raw != "" {
			fmt.Sscanf(raw, "%d", &page)
			seen = append(seen, page)
		}
		body, ok := pages[page]
		if !ok {
			body = ledgerPage(0)
		}
		_, _ = io.WriteString(w, body)
	})
	return c, &seen
}

// TestCreditExpiryPicksEarliestLiveBatch：只认 credit_type 1/2、剩余 > 0 且未过期。
func TestCreditExpiryPicksEarliestLiveBatch(t *testing.T) {
	now := time.Now()
	live := func(ms int64) string { return fmt.Sprintf("%d", ms) }
	day := int64(24 * time.Hour / time.Millisecond)

	c, _ := expiryClient(t, map[int]string{
		1: ledgerPage(6,
			// 有效：签到积分，3 天后到期 —— 应是答案
			`{"credit_type":2,"remaining_amount":"300.00","expire_at_ms":`+live(now.UnixMilli()+3*day)+`}`,
			// 有效但更晚：购买积分，10 天后
			`{"credit_type":1,"remaining_amount":"500.00","expire_at_ms":`+live(now.UnixMilli()+10*day)+`}`,
			// 已用完：剩余 0，就算最早也不算
			`{"credit_type":2,"remaining_amount":"0","expire_at_ms":`+live(now.UnixMilli()+day)+`}`,
			// 已过期：不能算（否则会显示一个过去的日期）
			`{"credit_type":2,"remaining_amount":"100","expire_at_ms":`+live(now.UnixMilli()-day)+`}`,
			// 未知类型：语义不明，不参与筛选
			`{"credit_type":9,"remaining_amount":"100","expire_at_ms":`+live(now.UnixMilli()+day/2)+`}`,
			// 无到期时间：0 表示不过期或未知
			`{"credit_type":1,"remaining_amount":"100","expire_at_ms":0}`,
		),
	})

	remain, items, err := c.UserResourceDetail(testAuth())
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if remain != 1100 {
		t.Errorf("余额应仍按 Σ remaining_amount = 1100，实际 %d", remain)
	}
	if len(items) != 1 {
		t.Fatalf("期望 1 条明细，实际 %+v", items)
	}
	want := time.UnixMilli(now.UnixMilli() + 3*day).In(time.FixedZone("CST", 8*3600)).Format(time.RFC3339)
	if items[0].ExpireAt != want {
		t.Errorf("ExpireAt = %q，期望最早有效批次 %q", items[0].ExpireAt, want)
	}
}

// TestCreditExpiryEmptyWhenAllBatchesDead：全过期 / 全用完 ⇒ 空串，不是零值时间。
func TestCreditExpiryEmptyWhenAllBatchesDead(t *testing.T) {
	now := time.Now().UnixMilli()
	c, _ := expiryClient(t, map[int]string{
		1: ledgerPage(2,
			`{"credit_type":2,"remaining_amount":"0","expire_at_ms":`+fmt.Sprintf("%d", now+86400000)+`}`,
			`{"credit_type":1,"remaining_amount":"10","expire_at_ms":`+fmt.Sprintf("%d", now-86400000)+`}`,
		),
	})
	_, items, err := c.UserResourceDetail(testAuth())
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if items[0].ExpireAt != "" {
		t.Errorf("无有效批次时 ExpireAt 必须是空串（前端据此隐藏有效期列），实际 %q", items[0].ExpireAt)
	}
}

// TestCreditExpiryPagesUntilComplete：账本跨页，必须翻完再取最小值。
func TestCreditExpiryPagesUntilComplete(t *testing.T) {
	now := time.Now().UnixMilli()
	day := int64(86400000)
	pages := map[int]string{
		// 第一页 total_count=2 但只给 1 条：不能就此停手
		1: ledgerPage(2, `{"credit_type":2,"remaining_amount":"1","expire_at_ms":`+fmt.Sprintf("%d", now+20*day)+`}`),
		2: ledgerPage(2, `{"credit_type":2,"remaining_amount":"1","expire_at_ms":`+fmt.Sprintf("%d", now+5*day)+`}`),
	}
	c, seen := expiryClient(t, pages)
	_, items, err := c.UserResourceDetail(testAuth())
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if len(*seen) < 2 {
		t.Fatalf("必须翻到第 2 页，实际请求页 %v", *seen)
	}
	want := time.UnixMilli(now + 5*day).In(time.FixedZone("CST", 8*3600)).Format(time.RFC3339)
	if items[0].ExpireAt != want {
		t.Errorf("跨页后应取全局最早 %q，实际 %q", want, items[0].ExpireAt)
	}
}

// TestCreditExpiryGivesUpOnIncompleteLedger：账本读不全就整体放弃。
//
// 只看到半份账本会算出「偏早」的到期日——比没有到期时间更糟：它会让人以为
// 积分快过期了，实际还有别的批次在后面。
func TestCreditExpiryGivesUpOnIncompleteLedger(t *testing.T) {
	now := time.Now().UnixMilli()
	c, _ := expiryClient(t, map[int]string{
		// total_count=5 但永远只回 1 条，翻满 10 页也读不全
		1: ledgerPage(5, `{"credit_type":2,"remaining_amount":"1","expire_at_ms":`+fmt.Sprintf("%d", now+86400000)+`}`),
	})
	_, items, err := c.UserResourceDetail(testAuth())
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if items[0].ExpireAt != "" {
		t.Errorf("账本未读全必须放弃到期结论，实际 %q", items[0].ExpireAt)
	}
}

// TestCreditExpiryMissingTotalCountGivesUp：没有 total_count 就无法判断是否读完。
func TestCreditExpiryMissingTotalCountGivesUp(t *testing.T) {
	now := time.Now().UnixMilli()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"base_resp":{"status_code":0},"details":[`+
			`{"credit_type":2,"remaining_amount":"1","expire_at_ms":`+fmt.Sprintf("%d", now+86400000)+`}]}`)
	})
	_, items, err := c.UserResourceDetail(testAuth())
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if items[0].ExpireAt != "" {
		t.Errorf("缺 total_count 必须放弃到期结论，实际 %q", items[0].ExpireAt)
	}
}

// TestCreditExpiryFailureDoesNotBreakBalance：账本读失败不得影响余额读取。
//
// 这是「降级为空」的核心约束：余额是主体功能，到期时间是附属信息，
// 附属信息失败必须闭嘴，不能把整条明细拖成错误。
func TestCreditExpiryFailureDoesNotBreakBalance(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page_number") != "" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, `{"base_resp":{"status_code":0},"total_count":1,"details":[{"remaining_amount":"750.00"}]}`)
	})
	remain, items, err := c.UserResourceDetail(testAuth())
	if err != nil {
		t.Fatalf("账本读失败不该让明细整体报错：%v", err)
	}
	if remain != 750 || items[0].Remain != 750 {
		t.Errorf("余额必须照常返回 750，实际 %d / %+v", remain, items)
	}
	if items[0].ExpireAt != "" {
		t.Errorf("读失败时 ExpireAt 应为空串，实际 %q", items[0].ExpireAt)
	}
}

// TestCreditExpiryCachesCompleteRead：读全了要缓存，挡住明细页的高频刷新。
func TestCreditExpiryCachesCompleteRead(t *testing.T) {
	now := time.Now().UnixMilli()
	c, seen := expiryClient(t, map[int]string{
		1: ledgerPage(1, `{"credit_type":1,"remaining_amount":"1","expire_at_ms":`+fmt.Sprintf("%d", now+86400000)+`}`),
	})
	for i := 0; i < 3; i++ {
		if _, _, err := c.UserResourceDetail(testAuth()); err != nil {
			t.Fatalf("第 %d 次 UserResourceDetail: %v", i, err)
		}
	}
	/* 余额请求每次都打，账本只该打一次 */
	ledgerHits := 0
	for _, p := range *seen {
		if p > 0 {
			ledgerHits++
		}
	}
	if ledgerHits != 1 {
		t.Errorf("账本应只读 1 次（其余命中缓存），实际 %d 次", ledgerHits)
	}
}

// TestCreditExpiryDoesNotCacheFailure：读失败不缓存，下一轮必须立刻重试。
func TestCreditExpiryDoesNotCacheFailure(t *testing.T) {
	now := time.Now().UnixMilli()
	fail := true
	var ledgerHits int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page_number") == "" {
			_, _ = io.WriteString(w, `{"base_resp":{"status_code":0},"total_count":0}`)
			return
		}
		ledgerHits++
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, ledgerPage(1,
			`{"credit_type":2,"remaining_amount":"1","expire_at_ms":`+fmt.Sprintf("%d", now+86400000)+`}`))
	})

	if _, _, err := c.UserResourceDetail(testAuth()); err != nil {
		t.Fatalf("首次: %v", err)
	}
	fail = false
	_, items, err := c.UserResourceDetail(testAuth())
	if err != nil {
		t.Fatalf("第二次: %v", err)
	}
	if ledgerHits != 2 {
		t.Errorf("失败结果不得缓存，第二轮必须重读账本，实际账本请求 %d 次", ledgerHits)
	}
	if items[0].ExpireAt == "" {
		t.Error("第二轮应能拿到到期时间")
	}
}

// TestCreditLedgerQueryShape：查询参数决定上游分页语义，写错就会静默读到错页。
func TestCreditLedgerQueryShape(t *testing.T) {
	q, err := parseQueryString(creditLedgerQuery(3))
	if err != nil {
		t.Fatalf("creditLedgerQuery: %v", err)
	}
	want := map[string]string{
		"device_platform": "web",
		"is_desktop":      "1",
		"desktop_version": "0.6.12",
		"page_number":     "3",
		"page_size":       "20",
		"include_expired": "true",
	}
	for k, v := range want {
		if q[k] != v {
			t.Errorf("%s = %q，期望 %q", k, q[k], v)
		}
	}
}

// TestLedgerAmountTolerant：账本金额是「带千分位的十进制字符串」。
func TestLedgerAmountTolerant(t *testing.T) {
	cases := []struct {
		raw  string
		want float64
		ok   bool
	}{
		{`"800.00"`, 800, true},
		{`"1,234.50"`, 1234.5, true},
		{`200`, 200, true},
		{`"-5"`, 0, true},   // 负数按 0：不参与筛选，但不是解析失败
		{`""`, 0, false},    // 空串：无法判断，跳过
		{`"abc"`, 0, false}, // 非数字：跳过
		{`null`, 0, false},  // 缺失：跳过
	}
	for _, c := range cases {
		got, ok := ledgerAmount(json.RawMessage(c.raw))
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ledgerAmount(%s) = (%v,%v)，期望 (%v,%v)", c.raw, got, ok, c.want, c.ok)
		}
	}
}

// TestExpireAtStringIsRFC3339CST：空值必须是空串，非空必须是 UTC+8 的 RFC3339。
func TestExpireAtStringIsRFC3339CST(t *testing.T) {
	if s := expireAtString(time.Time{}); s != "" {
		t.Errorf("零值时间必须序列化成空串，实际 %q", s)
	}
	// 2026-12-12T00:00:00+08:00 == 1797004800000ms
	got := expireAtString(time.UnixMilli(1797004800000))
	if got != "2026-12-12T00:00:00+08:00" {
		t.Errorf("expireAtString = %q，期望 2026-12-12T00:00:00+08:00", got)
	}
}

// parseQueryString 把 query 拆成 map（只用于本文件的断言，避免引 net/url 到断言里）。
func parseQueryString(raw string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range splitAmp(raw) {
		k, v, ok := cutEq(pair)
		if !ok {
			continue
		}
		out[k] = v
	}
	return out, nil
}

func splitAmp(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '&' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func cutEq(s string) (string, string, bool) {
	for i, r := range s {
		if r == '=' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

// TestCreditExpiryRefreshesAfterTTL：到期结论有 TTL，过期后必须重读账本。
//
// 不重读的后果是「积分早就续了一批，面板还挂着上月那批的到期日」——比没有更误导。
func TestCreditExpiryRefreshesAfterTTL(t *testing.T) {
	now := time.Now().UnixMilli()
	var ledgerHits int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page_number") == "" {
			_, _ = io.WriteString(w, `{"base_resp":{"status_code":0},"total_count":0}`)
			return
		}
		ledgerHits++
		_, _ = io.WriteString(w, ledgerPage(1,
			`{"credit_type":2,"remaining_amount":"1","expire_at_ms":`+fmt.Sprintf("%d", now+86400000)+`}`))
	})
	a := testAuth()
	if _, _, err := c.UserResourceDetail(a); err != nil {
		t.Fatalf("首次: %v", err)
	}
	if ledgerHits != 1 {
		t.Fatalf("首次应读 1 次账本，实际 %d", ledgerHits)
	}

	// 把缓存条目的读取时刻拨到 TTL 之外：模拟 5 分钟后再查。
	key := expiryCacheKey(tokenOf(a))
	c.expMu.Lock()
	e := c.expiries[key]
	e.fetched = time.Now().Add(-creditExpiryTTL - time.Minute)
	c.expiries[key] = e
	c.expMu.Unlock()

	if _, _, err := c.UserResourceDetail(a); err != nil {
		t.Fatalf("二次: %v", err)
	}
	if ledgerHits != 2 {
		t.Errorf("TTL 过期后必须重读账本（否则面板会一直显示旧到期日），实际 %d 次", ledgerHits)
	}
}

// TestCreditExpiryIsolatedPerToken：缓存按 access_token 分桶，换号不得复用。
//
// 两个账号的积分批次完全不同，共用一份缓存会把 A 的到期日显示到 B 身上。
func TestCreditExpiryIsolatedPerToken(t *testing.T) {
	now := time.Now().UnixMilli()
	var ledgerHits int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page_number") == "" {
			_, _ = io.WriteString(w, `{"base_resp":{"status_code":0},"total_count":0}`)
			return
		}
		ledgerHits++
		_, _ = io.WriteString(w, ledgerPage(1,
			`{"credit_type":2,"remaining_amount":"1","expire_at_ms":`+fmt.Sprintf("%d", now+86400000)+`}`))
	})
	a1 := testAuth()
	a2 := testAuth()
	a2.AccessToken = "mmoat_ANOTHER_account_token"

	if _, _, err := c.UserResourceDetail(a1); err != nil {
		t.Fatalf("账号 1: %v", err)
	}
	if _, _, err := c.UserResourceDetail(a2); err != nil {
		t.Fatalf("账号 2: %v", err)
	}
	if ledgerHits != 2 {
		t.Errorf("不同 token 必须各自读账本，实际只读 %d 次（缓存串号了）", ledgerHits)
	}
}
