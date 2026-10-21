package server

// strategy_sticky_test.go 挑号策略与粘性路由的交互。
//
// 粘性路由会把同一账号连用最多 50 次，而 roundrobin 的诉求是「每次都换一个」。
// 若不旁路粘性，roundrobin 选了也白选 —— 连续 50 个请求仍打同一个号，
// 用户看到的就是「策略没生效」。

import (
	"testing"

	"omnigate/internal/auth"
	"omnigate/internal/pool"
	"omnigate/internal/provider"
)

// newStrategyPool 造一个 3 账号的池（同分：靠 UID tie-break 保证顺序确定）。
func newStrategyPool(t *testing.T) *pool.Pool {
	t.Helper()
	p := pool.New(t.TempDir() + "/state.json")
	for _, uid := range []string{"A", "B", "C"} {
		p.Add(&auth.Auth{Kind: "glm", AccessToken: "at-" + uid, ExpiresAt: 4102444800, UID: uid})
		p.SetCreditDetail(uid, 1000, 0, 0)
	}
	return p
}

func newStrategyHandler(p *pool.Pool) *Handler {
	return NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.GLM: {Kind: provider.GLM, Pool: p, Upstream: &rotatingUpstream{},
				StaticModels: []provider.ModelInfo{{ID: "m"}}},
		},
		APIKey: "",
	})
}

// TestRoundRobinBypassesStickyRouting roundrobin 下每次请求都必须换号。
func TestRoundRobinBypassesStickyRouting(t *testing.T) {
	p := newStrategyPool(t)
	p.SetStrategy(pool.StrategyRoundRobin)
	h := newStrategyHandler(p)
	rt := h.cfg.Runtimes[provider.GLM]

	var seq []string
	for i := 0; i < 6; i++ {
		acct := h.pickWithSticky(rt, "m")
		if acct == nil {
			t.Fatalf("第%d次挑不到账号", i+1)
		}
		seq = append(seq, acct.UID)
	}
	for i := 1; i < len(seq); i++ {
		if seq[i] == seq[i-1] {
			t.Fatalf("roundrobin 未生效，第%d次与第%d次都是 %s（序列 %v）", i+1, i, seq[i-1], seq)
		}
	}

	// 切回默认策略后，不应残留 roundrobin 期间被旁路掉的粘性记录：
	// 默认策略必须恢复「连续命中同一账号」的行为。
	p.SetStrategy(pool.StrategyExpire)
	first := h.pickWithSticky(rt, "m").UID
	for i := 0; i < 5; i++ {
		if got := h.pickWithSticky(rt, "m").UID; got != first {
			t.Fatalf("默认策略应启用粘性（第%d次换到了 %s，起点 %s）", i+1, got, first)
		}
	}
}

// TestDefaultStrategyKeepsStickyRouting 默认（expire）策略下粘性必须保持原语义 ——
// 加策略不能顺带削弱原有的账号复用（省 token 上下文、减少刷新）。
func TestDefaultStrategyKeepsStickyRouting(t *testing.T) {
	p := newStrategyPool(t)
	h := newStrategyHandler(p)
	rt := h.cfg.Runtimes[provider.GLM]

	first := h.pickWithSticky(rt, "m").UID
	for i := 0; i < 10; i++ {
		if got := h.pickWithSticky(rt, "m").UID; got != first {
			t.Fatalf("默认策略应保持粘性（第%d次换到了 %s，起点 %s）", i+1, got, first)
		}
	}
}
