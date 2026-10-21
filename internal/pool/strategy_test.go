package pool

// strategy_test.go 挑号策略的回归测试。
//
// 重点锁定两件事：
//  1. **默认策略必须与基线一致**（expire：临期降序 → credits 降序）——
//     升级到可配置不能顺带改掉默认选号结果，否则用户会莫名其妙换了账号；
//  2. roundrobin 必须真正轮转，且游标落盘后重启继续（否则每次重启都从头轮）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"omnigate/internal/auth"
)

// mkPool 造一个含指定账号的池（uid → credits/expiring）。
func mkPool(t *testing.T, accounts map[string][2]int64, st Strategy) *Pool {
	t.Helper()
	p := New("")
	for uid, v := range accounts {
		p.Add(&auth.Auth{UID: uid})
		e := p.byUID[uid]
		e.credits, e.expiring = v[0], v[1]
	}
	p.SetStrategy(st)
	return p
}

// TestDefaultStrategyIsExpire 默认策略必须是 expire（= 基线硬编码的临期优先），
// 避免「加了可配置」的同时悄悄改变默认选号行为。
func TestDefaultStrategyIsExpire(t *testing.T) {
	p := New("")
	if got := p.GetStrategy(); got != StrategyExpire {
		t.Fatalf("默认策略=%q，应为 expire", got)
	}
	if got, ok := ParseStrategy(""); !ok || got != StrategyExpire {
		t.Fatalf("空串解析=%q/%v，应为 expire/true", got, ok)
	}
}

// TestParseStrategy 合法值与非法值的处理：非法必须回落默认并返回 false，
// 不能返回一个"看起来合法"的值。
func TestParseStrategy(t *testing.T) {
	for _, in := range []string{"credits", "CREDITS", " expire ", "roundrobin"} {
		got, ok := ParseStrategy(in)
		want := strings.ToLower(strings.TrimSpace(in))
		if !ok || string(got) != want {
			t.Fatalf("ParseStrategy(%q)=%q/%v，want %q/true", in, got, ok, want)
		}
	}
	for _, in := range []string{"none", "expire2", "轮询"} {
		got, ok := ParseStrategy(in)
		if ok || got != StrategyExpire {
			t.Fatalf("ParseStrategy(%q)=%q/%v，非法值应回落 expire 并返回 false", in, got, ok)
		}
	}
}

// TestExpireStrategyBaselineOrder expire 策略的比较键必须与基线一致：
// 临期降序 → credits 降序。临期为 0 但余额更高的账号排在临期账号之后。
func TestExpireStrategyBaselineOrder(t *testing.T) {
	p := mkPool(t, map[string][2]int64{
		"a": {5000, 0}, // 余额高但无临期
		"b": {100, 80}, // 余额低但有临期 → 必须先被选中
		"c": {300, 0},  // 无临期
	}, StrategyExpire)

	got := p.Pick().UID
	if got != "b" {
		t.Fatalf("expire 策略选中 %q，应为 b（有临期）", got)
	}
}

// TestCreditsStrategy 纯按可用余额排序，忽略临期。
func TestCreditsStrategy(t *testing.T) {
	p := mkPool(t, map[string][2]int64{
		"a": {5000, 0},
		"b": {100, 8000}, // 临期极高但余额低 → credits 策略必须无视
		"c": {300, 0},
	}, StrategyCredits)
	got := p.Pick().UID
	if got != "a" {
		t.Fatalf("credits 策略选中 %q，应为 a（余额最多）", got)
	}
}

// TestStrategyTieBreakStable 同分账号必须稳定（取 UID 最小者），
// 否则 map 遍历顺序会让每次刷新面板看到不同的"当前账号"。
func TestStrategyTieBreakStable(t *testing.T) {
	for _, st := range []Strategy{StrategyCredits, StrategyExpire} {
		for i := 0; i < 20; i++ {
			p := mkPool(t, map[string][2]int64{"z": {100, 0}, "m": {100, 0}, "a": {100, 0}}, st)
			if got := p.Pick().UID; got != "a" {
				t.Fatalf("策略 %s 第%d次选中 %q，应稳定为 a", st, i, got)
			}
		}
	}
}

// TestRoundRobinCycles roundrobin 必须依次选到每个账号并回环。
func TestRoundRobinCycles(t *testing.T) {
	p := mkPool(t, map[string][2]int64{"a": {10, 0}, "b": {9999, 0}, "c": {5000, 0}}, StrategyRoundRobin)
	var seq []string
	for i := 0; i < 6; i++ {
		seq = append(seq, p.Pick().UID)
	}
	want := []string{"a", "b", "c", "a", "b", "c"}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("轮询序列 %v，应为 %v", seq, want)
		}
	}
}

// TestRoundRobinCursorPersisted 游标落盘：重启后继续从上次位置的下一个轮，
// 而不是从头开始（否则每次重启都会偏回最低 UID）。
func TestRoundRobinCursorPersisted(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	for _, uid := range []string{"a", "b", "c"} {
		p.Add(&auth.Auth{UID: uid})
	}
	p.SetStrategy(StrategyRoundRobin)

	first := []string{p.Pick().UID, p.Pick().UID} // a, b
	if first[0] != "a" || first[1] != "b" {
		t.Fatalf("首轮应从最小 UID 起：%v", first)
	}
	// 游标应已落盘
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("轮询后未落盘: %v", err)
	}
	if !contains(string(raw), `"rr_last": "b"`) {
		t.Fatalf("state 未记录 rr_last: %s", raw)
	}

	p2 := New(fp)
	for _, uid := range []string{"a", "b", "c"} {
		p2.Add(&auth.Auth{UID: uid})
	}
	p2.SetStrategy(StrategyRoundRobin)
	if got := p2.Pick().UID; got != "c" {
		t.Fatalf("重启后首个选中 %q，应为 c（上次是 b 的下一个）", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestRoundRobinSkipsDisabled 轮转必须跳过被停用/冷却的账号，
// 游标只在实际选中时推进（否则会卡在同一个位置上自转）。
func TestRoundRobinSkipsDisabled(t *testing.T) {
	p := mkPool(t, map[string][2]int64{"a": {10, 0}, "b": {20, 0}, "c": {30, 0}}, StrategyRoundRobin)
	p.Disable("a", "测试停用")
	got := []string{p.Pick().UID, p.Pick().UID, p.Pick().UID}
	for _, g := range got {
		if g == "a" {
			t.Fatalf("已停用账号被选中: %v", got)
		}
	}
	seen := map[string]bool{}
	for _, g := range got {
		seen[g] = true
	}
	if !seen["b"] || !seen["c"] {
		t.Fatalf("轮转应覆盖 b/c: %v", got)
	}
}

// TestPickExcludingDrainsToNil tried 排空后必须返回 nil —— 这是 server 的
// MaxRotate 循环「已轮完一圈」的终止信号。若改为回退到全体账号，
// 循环会拿到已试过的账号继续跑，错误被重复触发且永不退出。
func TestPickExcludingDrainsToNil(t *testing.T) {
	p := mkPool(t, map[string][2]int64{"a": {10, 0}, "b": {20, 0}}, StrategyExpire)
	tried := map[string]bool{"a": true, "b": true}
	if got := p.PickExcluding(tried); got != nil {
		t.Fatalf("全部试完后应返回 nil（轮换循环的终止信号），实际 %+v", got)
	}
}

// TestPickForModelStillRespectsStrategy 模型级过滤与策略正交：
// 最高分账号在目标模型上冷却时，应选次优者。
func TestPickForModelStillRespectsStrategy(t *testing.T) {
	p := mkPool(t, map[string][2]int64{"a": {5000, 0}, "b": {100, 0}}, StrategyCredits)
	p.CooldownModel("a", "m1", time.Hour, "429")
	if got := p.PickForModel("m1"); got == nil || got.UID != "b" {
		t.Fatalf("m1 上 a 冷却，应选中 b，实际 %v", got)
	}
	// 其它模型不受影响
	if got := p.PickForModel("m2"); got == nil || got.UID != "a" {
		t.Fatalf("m2 上 a 仍可用，应选中 a，实际 %v", got)
	}
}

// TestEntryBetterKeepsBaseline 三级比较键（临期 → 余额 → UID）与 pickExpireLocked 一致。
func TestEntryBetterKeepsBaseline(t *testing.T) {
	mk := func(uid string, c, x int64) *entry {
		return &entry{a: &auth.Auth{UID: uid}, credits: c, expiring: x}
	}
	cases := []struct {
		a, b *entry
		want bool
	}{
		{mk("a", 1, 10), mk("b", 9999, 0), true},  // 临期优先压过余额
		{mk("a", 9999, 0), mk("b", 1, 10), false}, // 反向
		{mk("a", 100, 5), mk("b", 50, 5), true},   // 临期相同比余额
		{mk("a", 50, 5), mk("b", 100, 5), false},
		{mk("a", 50, 5), mk("b", 50, 5), true}, // 全同比 UID
		{mk("b", 50, 5), mk("a", 50, 5), false},
	}
	for i, c := range cases {
		if got := entryBetter(c.a, c.b); got != c.want {
			t.Fatalf("用例%d entryBetter=%v，want %v", i, got, c.want)
		}
	}
}
