package ledger

import (
	"testing"

	"omnigate/internal/provider"
)

// R97：InfoOnly 是纯展示条目，量纲可能不是「分」（monkeycode「每日 Token 额度」
// 即 token 数）。参与差分会产出假的 expire/spend，被临期列当成「今日作废」。
func TestR97DiffCreditsSkipsInfoOnly(t *testing.T) {
	l := newTestLedger(t)
	ch, uid := "monkeycode", "mc1"

	// 首轮：只有一条真实可消耗条目，建立基线（不产生事件）。
	l.DiffCredits(ch, uid, 100, []provider.ResourceItem{
		{Name: "月套餐", Remain: 100, Total: 100, Usable: true},
	})
	// 次轮：加入一条 InfoOnly 的巨量 token 条目；若它参与差分，会产出
	// earn 1,000,000 或后续 expire，把积分口径污染。
	n := l.DiffCredits(ch, uid, 100, []provider.ResourceItem{
		{Name: "月套餐", Remain: 100, Total: 100, Usable: true},
		{Name: "每日 Token 额度", Remain: 1000000, Total: 1000000, Usable: true, InfoOnly: true},
	})
	if n != 0 {
		t.Fatalf("InfoOnly 条目不应产生任何流水: events=%d, want 0", n)
	}
}

// 旧快照里已经误存了 InfoOnly 条目（旧版本写入的），新一轮必须清掉，
// 不能因为「上次有、本次消失」而记一笔假 expire。
func TestR97DiffCreditsCleansLegacyInfoOnlySnapshot(t *testing.T) {
	l := newTestLedger(t)
	ch, uid := "monkeycode", "mc1"
	info := provider.ResourceItem{Name: "每日 Token 额度", Remain: 500000, Total: 500000, Usable: true, InfoOnly: true}

	// 模拟旧版：把真实条目 + InfoOnly（去掉标记，当普通条目）一起写进快照，建立基线。
	// 两轮条目集合一致，差分本应为 0 事件。
	l.DiffCredits(ch, uid, 100, []provider.ResourceItem{
		{Name: "月套餐", Remain: 100, Total: 100, Usable: true},
		{Name: info.Name, Remain: info.Remain, Total: info.Total, Usable: true},
	})
	// 新版拿到带 InfoOnly 的同一条目：旧快照里的 InfoOnly 项必须被清掉，
	// 不得产出「消失 ⇒ expire 500000」。
	n := l.DiffCredits(ch, uid, 100, []provider.ResourceItem{
		{Name: "月套餐", Remain: 100, Total: 100, Usable: true},
		info,
	})
	if n != 0 {
		t.Fatalf("旧快照 InfoOnly 残留应被清理: events=%d, want 0", n)
	}
	st := l.Query(1, nil)
	for _, e := range st.Credit.Entries {
		if e.Kind == "expire" && e.Amount <= -100000 {
			t.Fatalf("出现假的巨额 expire 流水: %+v", e)
		}
	}
}
