// credit_pool_shrink_test.go R101 回归：整包到期/回收不得被记成「今日消耗」。
//
// 生产实录（QoderCN）：一个 100 分的赠送包整包到期消失，而它此前已被花掉 1 分。
// 上游回包 total 2500→2400、used 1→0、remaining 2499→2400 ⇒ 旧账本按
// 「余额下降 + 无到期日」记成 spend(-99)，面板显示成今日消耗，实际当天真消耗是 0。
//
// 修法：用恒等式 remain = total - used 拆项，Δremain = Δtotal - ΔUsed，
// 并用三道门槛（无到期日 / 渠道分别上报 total 与 used / total 确实下降）圈定适用面。
package ledger

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy-api/internal/provider"
)

// lastCreditEntry 读当月流水里最后一条（测试里少写几行）。
func lastCreditEntry(t *testing.T, l *Ledger, dir string) CreditEntry {
	t.Helper()
	l.Flush()
	raw, err := os.ReadFile(filepath.Join(dir, monthFile("credit", time.Now())))
	if err != nil {
		t.Fatalf("read credit: %v", err)
	}
	var last CreditEntry
	for _, line := range splitLines(raw) {
		_ = unmarshal(line, &last)
	}
	return last
}

// TestPoolShrankExpiryNotSpend 整包到期（total 下降 + used 归零）应归因 expire 而非 spend。
func TestPoolShrankExpiryNotSpend(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	l, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// 首轮：total=2500 used=1 remain=2499，且**无到期日**（上游不给包级到期时间）。
	cur1 := []provider.ResourceItem{
		{Name: "赠送包", Key: "gift", Remain: 2499, Used: 1, Total: 2500, Usable: true},
	}
	l.DiffCredits("qodercn", "u1", 2499, cur1)

	// 次轮：整包到期被回收 ⇒ total 2400 / used 0 / remain 2400。
	cur2 := []provider.ResourceItem{
		{Name: "赠送包", Key: "gift", Remain: 2400, Used: 0, Total: 2400, Usable: true},
	}
	l.DiffCredits("qodercn", "u1", 2400, cur2)

	if last := lastCreditEntry(t, l, dir); last.Kind != "expire" {
		t.Fatalf("整包到期应归因 expire，got %s (amount=%d)", last.Kind, last.Amount)
	}
}

// TestPoolShrankFalsePositiveGuards 三道门槛缺一不可，任一不满足都退回 spend。
func TestPoolShrankFalsePositiveGuards(t *testing.T) {
	cases := []struct {
		name       string
		prev       itemSnap
		cur        int64
		booksUsage bool
		want       bool
	}{
		{
			name: "门槛1 有到期日的条目走既有 expired 判据",
			prev: itemSnap{Total: 2500, Expire: "2099-01-01"},
			cur:  2400, booksUsage: true, want: false,
		},
		{
			name: "门槛2 渠道不分别上报 total/used（Total 是 Remain 的拷贝）",
			prev: itemSnap{Total: 2500},
			cur:  2400, booksUsage: false, want: false,
		},
		{
			name: "门槛3 老快照无 total（t 字段缺失）",
			prev: itemSnap{},
			cur:  2400, booksUsage: true, want: false,
		},
		{
			name: "门槛3 total 未下降（真实消耗）",
			prev: itemSnap{Total: 2500},
			cur:  2500, booksUsage: true, want: false,
		},
		{
			name: "三门槛全过",
			prev: itemSnap{Total: 2500},
			cur:  2400, booksUsage: true, want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := poolShrankExpiry(c.prev, c.cur, c.booksUsage); got != c.want {
				t.Errorf("poolShrankExpiry=%v want %v", got, c.want)
			}
		})
	}
}

// TestRealSpendStillSpend 真实消耗（total 不变、used 变大）必须仍记 spend。
func TestRealSpendStillSpend(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	l, _ := New(dir)
	defer l.Close()

	cur1 := []provider.ResourceItem{
		{Name: "赠送包", Key: "gift", Remain: 2499, Used: 1, Total: 2500, Usable: true},
	}
	l.DiffCredits("qodercn", "u2", 2499, cur1)
	// 花掉 50：total 仍 2500，used 1→51，remain 2499→2449。
	cur2 := []provider.ResourceItem{
		{Name: "赠送包", Key: "gift", Remain: 2449, Used: 51, Total: 2500, Usable: true},
	}
	l.DiffCredits("qodercn", "u2", 2449, cur2)

	if last := lastCreditEntry(t, l, dir); last.Kind != "spend" {
		t.Fatalf("真实消耗应记 spend，got %s", last.Kind)
	}
}

// TestChannelBooksUsageSharedAcrossAccounts 门槛 2 的渠道级标记跨账号共享：
// A 号花过钱（used>0）后，只签到不花钱的 B 号（used 恒 0）整包到期也应归因 expire。
// 按 key 判会漏掉 B 号——这正是渠道级标记存在的理由。
func TestChannelBooksUsageSharedAcrossAccounts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	l, _ := New(dir)
	defer l.Close()

	// A 号：used>0 ⇒ 首次刷新即证实「本渠道分别上报 total 与 used」，写入 @ch 标记。
	l.DiffCredits("qodercn", "acctA", 2499, []provider.ResourceItem{
		{Name: "包", Key: "gift", Remain: 2499, Used: 7, Total: 2500, Usable: true},
	})

	// B 号：used 恒 0（只签到不花钱），整包到期。
	l.DiffCredits("qodercn", "acctB", 2400, []provider.ResourceItem{
		{Name: "包", Key: "gift", Remain: 2400, Used: 0, Total: 2400, Usable: true},
	})
	l.DiffCredits("qodercn", "acctB", 2400, []provider.ResourceItem{
		{Name: "包", Key: "gift", Remain: 2300, Used: 0, Total: 2300, Usable: true},
	})

	l.Flush()
	raw, _ := os.ReadFile(filepath.Join(dir, monthFile("credit", time.Now())))
	kinds := map[string]int{}
	for _, line := range splitLines(raw) {
		var e CreditEntry
		_ = unmarshal(line, &e)
		kinds[e.UID+"/"+e.Kind]++
	}
	// B 号第二轮到 2300：total 2400→2300 下降 ⇒ 应记 expire（若有 @ch 标记）。
	if kinds["acctB/expire"] == 0 {
		t.Fatalf("B 号整包到期应记 expire（B 自身 used 恒 0，靠 A 号留下的渠道标记），流水=%v", kinds)
	}

	// 快照里应存在渠道级保留键。
	raw2, _ := os.ReadFile(filepath.Join(dir, "credit-snapshot.json"))
	var snap snapshots
	_ = unmarshal(raw2, &snap)
	if !channelBooksUsage(snap, "qodercn") {
		t.Fatalf("快照应写入 %s/qodercn 渠道级标记，实际键=%v", snapChannels, keysOf(snap))
	}
}

func keysOf(m snapshots) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
