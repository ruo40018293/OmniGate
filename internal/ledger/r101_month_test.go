// r101_ledger_month_test.go 锁住 R101：月末裁剪 cutoff 不能溢出。
//
// 原实现 `time.Now().AddDate(0, -keepMonths, 0).Format("200601")`：
// 10 月 31 日减 1 个月 ⇒ Go 的 AddDate 得到「9 月 31 日」，规整后变成 10 月 1 日
// ⇒ cutoff = "202610" = **当月**，于是 `stem < cutoff` 把所有更早的分段（含 9 月）
// 一起删掉，保留月数形同虚设 —— 每月的月末几天都会把上个月的流水清空。
//
// 修法：先把当前时间归到当月 1 号再减，这样任何月份都不存在溢出。
package ledger

import (
	"testing"
	"time"
)

// TestMonthTagShiftNoOverflow 月末日期减月不得溢出到下个月。
func TestMonthTagShiftNoOverflow(t *testing.T) {
	cases := []struct {
		now  time.Time
		back int
		want string
		why  string
	}{
		{time.Date(2026, 10, 31, 0, 0, 0, 0, time.Local), 1, "202609", "10/31 减 1 月：原实现会得 202610（连当月一起删）"},
		{time.Date(2026, 3, 31, 0, 0, 0, 0, time.Local), 1, "202602", "3/31 减 1 月：原实现会得 202603"},
		{time.Date(2026, 5, 31, 0, 0, 0, 0, time.Local), 1, "202604", "5/31 减 1 月：原实现会得 202605"},
		{time.Date(2026, 12, 31, 0, 0, 0, 0, time.Local), 1, "202611", "12/31 减 1 月"},
		{time.Date(2026, 1, 31, 0, 0, 0, 0, time.Local), 1, "202512", "1/31 减 1 月跨年"},
		{time.Date(2026, 10, 15, 0, 0, 0, 0, time.Local), 1, "202609", "月中日期（常规路径）"},
		{time.Date(2026, 10, 31, 0, 0, 0, 0, time.Local), 0, "202610", "back=0 就是当月"},
		{time.Date(2026, 10, 31, 0, 0, 0, 0, time.Local), 3, "202607", "减 3 个月"},
		{time.Date(2026, 2, 28, 0, 0, 0, 0, time.Local), 1, "202601", "2/28 减 1 月"},
		{time.Date(2024, 2, 29, 0, 0, 0, 0, time.Local), 1, "202401", "闰年 2/29 减 1 月"},
	}
	for _, tc := range cases {
		if got := monthTagShift(tc.now, tc.back); got != tc.want {
			t.Errorf("monthTagShift(%s, %d) = %q，期望 %q（%s）",
				tc.now.Format("2006-01-02"), tc.back, got, tc.want, tc.why)
		}
	}
}

// TestMonthTagShiftNeverReturnsFuture 任何输入都不得回出比当月更晚的标签。
// 这是溢出 bug 的判据形态：溢出时 cutoff 变成当月，语义上等于「不许有上个月」。
func TestMonthTagShiftNeverReturnsFuture(t *testing.T) {
	// 覆盖连续 3 年的每一天
	for d := time.Date(2024, 1, 1, 0, 0, 0, 0, time.Local); d.Year() < 2027; d = d.AddDate(0, 0, 1) {
		want := time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, d.Location()).AddDate(0, -1, 0).Format("200601")
		if got := monthTagShift(d, 1); got != want {
			t.Fatalf("monthTagShift(%s, 1) = %q，期望 %q", d.Format("2006-01-02"), got, want)
		}
		if got := monthTagShift(d, 0); got != d.Format("200601") {
			t.Fatalf("monthTagShift(%s, 0) 必须等于当月，得到 %q", d.Format("2006-01-02"), got)
		}
	}
}
