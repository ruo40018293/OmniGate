package session

// session_test.go 会话粘性路由的回归测试。
//
// 重点锁四件事：
//  1. **同一会话恒定同号**，跨请求不漂移（这是整个功能的目的）；
//  2. **渠道隔离**（本项目是多渠道聚合，会话键不带渠道前缀会串号）；
//  3. 绑定号不可用时能重新分配，而不是钉死在坏号上；
//  4. GC 能真正停掉（stop channel 捕获到局部变量，否则关停失效 + goroutine 泄漏）。

import (
	"sync"
	"testing"
	"time"
)

// fixedAvailable 造一个固定可用列表的回调。
func fixedAvailable(uids ...string) func(string) []string {
	list := append([]string(nil), uids...)
	return func(string) []string { return list }
}

// TestSameKeySticksToSameAccount 同一会话反复解析必须恒定返回同一账号。
func TestSameKeySticksToSameAccount(t *testing.T) {
	r := New(Config{Available: fixedAvailable("a", "b", "c")})
	first, ok := r.Resolve("workbuddy", "conv-1")
	if !ok {
		t.Fatal("首次应能分配")
	}
	for i := 0; i < 20; i++ {
		got, ok := r.Resolve("workbuddy", "conv-1")
		if !ok || got != first {
			t.Fatalf("第%d次解析=%q，%v；应恒定为 %q", i+1, got, ok, first)
		}
	}
}

// TestDifferentKeysSpread 不同会话应分散到不同账号（双段策略的「空闲优先」生效）。
func TestDifferentKeysSpread(t *testing.T) {
	r := New(Config{Available: fixedAvailable("a", "b", "c")})
	seen := map[string]bool{}
	for i := 0; i < 12; i++ {
		uid, ok := r.Resolve("workbuddy", string(rune('a'+i))+"-conv")
		if !ok {
			t.Fatalf("第%d个会话分配失败", i)
		}
		seen[uid] = true
	}
	if len(seen) < 2 {
		t.Fatalf("12 个会话只用到 %d 个账号，未分散：%v", len(seen), seen)
	}
}

// TestChannelIsolation **渠道隔离**（多渠道聚合的关键不变量）。
//
// 回归场景：会话键不带渠道前缀时，traework 的会话可能被分配到 workbuddy 的账号 ——
// 那个 uid 在 traework 池里根本不存在，请求会 503。
func TestChannelIsolation(t *testing.T) {
	r := New(Config{
		// 按渠道给不同的账号集合（模拟各渠道独立账号池）
		Available: func(ch string) []string {
			if ch == "traework" {
				return []string{"tr-1", "tr-2"}
			}
			return []string{"wb-1", "wb-2", "wb-3"}
		},
	})
	wb, _ := r.Resolve("workbuddy", "wb|conv-1")
	tr, _ := r.Resolve("traework", "tr|conv-1")
	// 用前缀判定渠道归属（账号名形如 wb-N / tr-N）
	if !hasPrefix(wb, "wb-") {
		t.Errorf("workbuddy 会话分到了非本渠道账号：%q", wb)
	}
	if !hasPrefix(tr, "tr-") {
		t.Errorf("traework 会话分到了非本渠道账号：%q", tr)
	}
	if wb == tr {
		t.Errorf("两个渠道分到了同一账号：%q", wb)
	}
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

// TestRebindWhenAccountUnavailable 绑定号从可用列表消失后必须重新分配。
func TestRebindWhenAccountUnavailable(t *testing.T) {
	var avail = []string{"a", "b"}
	r := New(Config{Available: func(string) []string { return avail }})
	first, _ := r.Resolve("workbuddy", "conv-1")
	if first != "a" && first != "b" {
		t.Fatalf("分配到意外账号 %q", first)
	}
	// 把原账号移出可用集合（模拟被冷却/禁用）
	avail = []string{"c"}
	got, ok := r.Resolve("workbuddy", "conv-1")
	if !ok || got != "c" {
		t.Fatalf("原账号不可用后应重分配到 c，实际 %q(%v)", got, ok)
	}
}

// TestUnavailableYieldsNoAccount 可用列表为空时返回 false（调用方回落普通轮换）。
func TestUnavailableYieldsNoAccount(t *testing.T) {
	r := New(Config{Available: fixedAvailable()})
	if _, ok := r.Resolve("workbuddy", "conv-1"); ok {
		t.Fatal("无可用账号时应返回 ok=false")
	}
}

// TestEmptyKeyNoBinding 空键不绑定（客户端没发会话 id 的正常情形）。
func TestEmptyKeyNoBinding(t *testing.T) {
	r := New(Config{Available: fixedAvailable("a")})
	if _, ok := r.Resolve("workbuddy", ""); ok {
		t.Fatal("空会话键不应分配")
	}
	if r.Count() != 0 {
		t.Fatalf("空键不应留下绑定，Count=%d", r.Count())
	}
}

// TestTTLExpiryRebinds 绑定过期后重新分配（哈希稳定 → 仍分到同一号；
// 关键是过期条目被清掉，不会无限增长）。
func TestTTLExpiryRebinds(t *testing.T) {
	r := New(Config{TTL: time.Millisecond, Available: fixedAvailable("a", "b")})
	if _, ok := r.Resolve("workbuddy", "conv-1"); !ok {
		t.Fatal("首次分配失败")
	}
	time.Sleep(5 * time.Millisecond)
	if _, ok := r.Resolve("workbuddy", "conv-1"); !ok {
		t.Fatal("过期后应能重新分配")
	}
	// 直接验证 GC 能清掉过期项
	r.entries["old"] = entry{uid: "a", lastActive: time.Now().Add(-time.Hour)}
	if n := r.gcOnce(time.Now()); n != 1 {
		t.Fatalf("gcOnce 清理了 %d 条，应为 1", n)
	}
}

// TestBindUnbind Bind 覆盖旧值；Unbind 返回是否存在。
func TestBindUnbind(t *testing.T) {
	r := New(Config{Available: fixedAvailable("a", "b")})
	r.Bind("workbuddy|conv-1", "a")
	if uid, _ := r.Resolve("workbuddy", "workbuddy|conv-1"); uid != "a" {
		t.Fatalf("Bind 后应解析到 a，实际 %q", uid)
	}
	r.Bind("workbuddy|conv-1", "b") // 幂等覆盖
	if uid, _ := r.Resolve("workbuddy", "workbuddy|conv-1"); uid != "b" {
		t.Fatalf("覆盖后应解析到 b，实际 %q", uid)
	}
	if !r.Unbind("workbuddy|conv-1") {
		t.Error("Unbind 应返回 true")
	}
	if r.Unbind("workbuddy|conv-1") {
		t.Error("重复 Unbind 应返回 false")
	}
}

// TestBindIgnoresEmpty 空的 key/uid 不产生绑定（防止垃圾条目堆积）。
func TestBindIgnoresEmpty(t *testing.T) {
	r := New(Config{Available: fixedAvailable("a")})
	r.Bind("", "a")
	r.Bind("k", "")
	if r.Count() != 0 {
		t.Fatalf("空参数不应产生绑定，Count=%d", r.Count())
	}
}

// TestGCLifecycle GC 能启动也能真正停掉。
//
// 回归：stop channel 若在 goroutine 的 select 里每轮重新求值 r.stop，
// 与 StopGC 的写锁构成数据竞争，且读到 nil 后该 case 永久阻塞 → 关停失效、
// goroutine 泄漏。捕获局部变量后本测试才能通过。
func TestGCLifecycle(t *testing.T) {
	r := New(Config{GCInterval: time.Millisecond, Available: fixedAvailable("a")})
	r.StartGC()
	r.StartGC() // 幂等：不应起第二个 goroutine
	time.Sleep(10 * time.Millisecond)

	// R57-A：先证明 GC 真的在工作。GCInterval=1ms，上面 10ms 内必然触发过；
	// 显式塞一条过期项并等它被清掉，后面的「不再清理」断言才有意义
	// （否则一个从未工作的 goroutine 也能让这个测试永远绿）。
	r.entries["warm"] = entry{uid: "a", lastActive: time.Now().Add(-time.Hour)}
	deadline := time.Now().Add(300 * time.Millisecond)
	for {
		r.mu.RLock()
		_, alive := r.entries["warm"]
		r.mu.RUnlock()
		if !alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("GC 从未清理过过期项（goroutine 没在工作）")
		}
		time.Sleep(2 * time.Millisecond)
	}

	r.StopGC()
	r.StopGC() // 幂等
	// 停掉后不再清理：塞一条过期项，等两个周期应仍在。
	// 必须与 StopGC「同步」才成立 —— 若 StopGC 只发信号就返回，那一轮还在飞的
	// gcOnce 会把它删掉（2026-10-23 实测三次两红）。
	r.entries["z"] = entry{uid: "a", lastActive: time.Now().Add(-time.Hour)}
	time.Sleep(20 * time.Millisecond)
	if _, ok := r.entries["z"]; !ok {
		t.Error("StopGC 后不应再清理（说明 goroutine 没退出）")
	}
}

// TestStopGCIsSynchronous R57-A：StopGC 返回时 goroutine 必须已经退出。
// 判据：返回后立刻写入的过期项不被清理。单跑一次很难撞上窗口（实测 8 次 0 红），
// 所以放大到 200 轮 + 1ms GC 周期把竞争压到极限。
func TestStopGCIsSynchronous(t *testing.T) {
	for round := 0; round < 200; round++ {
		r := New(Config{GCInterval: time.Millisecond, Available: fixedAvailable("a")})
		r.StartGC()
		time.Sleep(time.Millisecond) // 让 goroutine 至少进过一次 select
		r.StopGC()
		r.entries["x"] = entry{uid: "a", lastActive: time.Now().Add(-time.Hour)}
		time.Sleep(3 * time.Millisecond)
		if _, ok := r.entries["x"]; !ok {
			t.Fatalf("第 %d 轮：StopGC 返回后仍有 gcOnce 在飞，删掉了刚写入的过期项", round)
		}
	}
}

// TestConcurrentResolveSameKey 并发同 key 不得重复分配出不同账号
// （TOCTOU 防护：写锁内必须 re-check）。
func TestConcurrentResolveSameKey(t *testing.T) {
	r := New(Config{Available: fixedAvailable("a", "b", "c", "d")})
	const n = 64
	var wg sync.WaitGroup
	got := make([]string, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			uid, _ := r.Resolve("workbuddy", "same-conv")
			got[i] = uid
		}(i)
	}
	wg.Wait()
	first := got[0]
	if first == "" {
		t.Fatal("并发解析未返回账号")
	}
	for i, g := range got {
		if g != first {
			t.Fatalf("并发同 key 分到了不同账号：got[%d]=%q first=%q", i, g, first)
		}
	}
	if r.Count() != 1 {
		t.Fatalf("并发同 key 应只留 1 条绑定，实际 %d", r.Count())
	}
}

// TestModelDimensionRebind 绑定号对某模型不可用时，按模型维度重新分配。
//
// 回归场景：账号被 6004 模型级限额后对其他模型仍可用；若只按账号级校验，
// 会话会被钉在这个号上反复失败（用户观感：「限额后换不动号」）。
func TestModelDimensionRebind(t *testing.T) {
	byModel := map[string][]string{
		"m1": {"a", "b"},
		"m2": {"b", "c"}, // a 在 m2 上不可用
	}
	r := New(Config{
		AvailableForModel: func(_, model string) []string {
			if model == "" {
				return []string{"a", "b", "c"}
			}
			return byModel[model]
		},
	})
	// m1 上绑定到某号后，切到 m2 应换一个（若原号不在 m2 可用集合）
	uid1, ok := r.ResolveForModel("workbuddy", "conv-1", "m1")
	if !ok {
		t.Fatal("m1 分配失败")
	}
	uid2, ok := r.ResolveForModel("workbuddy", "conv-1", "m2")
	if !ok {
		t.Fatal("m2 分配失败")
	}
	if uid2 == "a" {
		t.Errorf("a 在 m2 上不可用，不该被分配（m1=%q m2=%q）", uid1, uid2)
	}
}
