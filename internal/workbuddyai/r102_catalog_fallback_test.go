// r102_catalog_fallback_test.go v2 目录端点下线后的自动切换（2026-11-14 起
// /v2/enterprises/personal/models 恒返 404，见 models.go 的 v2Down 注释）。
//
// 锁四件事：
//  1. v2 404 → 自动切 v3，模型表不为空（否则整个渠道在面板消失）；
//  2. 切过一次后不再白撞 v2（否则每轮刷新都先吃一个 404）；
//  3. 非 404 的失败（401/网络）**不切换**（换端点救不回来，会掩盖账号过期）；
//  4. 窗口到期后重探 v2，恢复则切回（v2 是 cli agent 的权威顺序）。
package workbuddyai

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"omnigate/internal/provider"
)

// resetCatalogCache 清空全局目录缓存，避免用例之间互相污染。
func resetCatalogCache() {
	cache.mu.Lock()
	cache.models, cache.promo, cache.badges, cache.colors = nil, nil, nil, nil
	cache.fetched = time.Time{}
	cache.v2Down, cache.v2RetryAt = false, time.Time{}
	cache.v3, cache.v3OK, cache.v3Fetched = nil, false, time.Time{}
	cache.mu.Unlock()
}

// v2Body 构造一份合法的 v2 目录响应（cli agent 面 2 个模型）。
func v2CatalogBody() string {
	return `{"code":0,"data":{"models":[
		{"id":"v2-model-a","name":"A","credits":"x0.10","maxInputTokens":100000,"maxOutputTokens":10000,
		 "supportsImages":true,"supportsReasoning":true,"supportsToolCall":true},
		{"id":"v2-model-b","name":"B","credits":"x0.20","maxInputTokens":200000,"maxOutputTokens":20000,
		 "supportsImages":false,"supportsReasoning":false,"supportsToolCall":true}
	],"agents":[{"name":"cli","models":["v2-model-a","v2-model-b"]}]}}`
}

// v3CatalogBody 构造一份合法的 v3 目录响应（models[] 3 个、cli agent 2 个）。
func v3CatalogBody() string {
	return `{"code":0,"data":{"models":[
		{"id":"v3-model-a","name":"A3","credits":"x0.11","maxInputTokens":900000,"maxOutputTokens":64000,
		 "supportsImages":true,"supportsReasoning":true,"supportsToolCall":true},
		{"id":"v3-model-b","name":"B3","credits":"x0.22","maxInputTokens":500000,"maxOutputTokens":32000,
		 "supportsImages":false,"supportsReasoning":false,"supportsToolCall":true},
		{"id":"v3-extra","name":"E3","credits":"","maxInputTokens":0,"maxOutputTokens":0}
	],"agents":[{"name":"cli","models":["v3-model-a","v3-model-b"]}]}}`
}

// TestFetchCatalogFallsBackToV3On404 v2 404 时必须自动切 v3，模型表不得为空。
func TestFetchCatalogFallsBackToV3On404(t *testing.T) {
	resetCatalogCache()
	var v2Hits, v3Hits int
	srv := testServer(func(w http.ResponseWriter, r *http.Request, n int) {
		switch r.URL.Path {
		case EpCatalog:
			v2Hits++
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error_msg":"404 Route Not Found"}`))
		case EpCatalogV3:
			v3Hits++
			_, _ = w.Write([]byte(v3CatalogBody()))
		default:
			t.Errorf("意外请求路径 %s", r.URL.Path)
		}
	})
	defer srv.Close()
	c := New()
	c.Base = srv.URL

	infos, err := c.FetchModels(testAuth())
	if err != nil {
		t.Fatalf("v2 404 后应回退 v3 并成功，实际 err=%v", err)
	}
	if len(infos) == 0 {
		t.Fatal("回退 v3 后模型表不得为空（空表会让整个渠道在面板消失）")
	}
	if v2Hits != 1 || v3Hits == 0 {
		t.Fatalf("期望 v2 试 1 次、v3 至少 1 次，实际 v2=%d v3=%d", v2Hits, v3Hits)
	}
	found := false
	for _, m := range infos {
		if m.ID == "v3-model-a" {
			found = true
			if m.ContextWindow != 900000 {
				t.Fatalf("v3 的上下文窗口应被采纳，实际 %d", m.ContextWindow)
			}
		}
	}
	if !found {
		t.Fatalf("v3 的模型应出现在结果里，实际 %+v", ids(infos))
	}
}

// TestFetchCatalogRemembersV2Down 记住下线后不得每轮重撞 v2。
func TestFetchCatalogRemembersV2Down(t *testing.T) {
	resetCatalogCache()
	var v2Hits int
	srv := testServer(func(w http.ResponseWriter, r *http.Request, n int) {
		switch r.URL.Path {
		case EpCatalog:
			v2Hits++
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error_msg":"404 Route Not Found"}`))
		case EpCatalogV3:
			_, _ = w.Write([]byte(v3CatalogBody()))
		}
	})
	defer srv.Close()
	c := New()
	c.Base = srv.URL

	if _, err := c.FetchModels(testAuth()); err != nil {
		t.Fatalf("第一次: %v", err)
	}
	// 让 10 分钟目录缓存失效，但 v2 的 30 分钟下线窗口仍在。
	cache.mu.Lock()
	cache.fetched = time.Time{}
	cache.v3Fetched = time.Time{}
	cache.mu.Unlock()

	if _, err := c.FetchModels(testAuth()); err != nil {
		t.Fatalf("第二次: %v", err)
	}
	if v2Hits != 1 {
		t.Fatalf("判定 v2 下线后不应再白撞它，期望 v2 共 1 次，实际 %d 次", v2Hits)
	}
}

// TestFetchCatalogDoesNotFallbackOnAuthError 401 必须原样上抛，不得切源。
func TestFetchCatalogDoesNotFallbackOnAuthError(t *testing.T) {
	resetCatalogCache()
	var v3Hits int
	srv := testServer(func(w http.ResponseWriter, r *http.Request, n int) {
		switch r.URL.Path {
		case EpCatalog:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error_msg":"unauthorized"}`))
		case EpCatalogV3:
			v3Hits++
			_, _ = w.Write([]byte(v3CatalogBody()))
		}
	})
	defer srv.Close()
	c := New()
	c.Base = srv.URL

	_, err := c.FetchModels(testAuth())
	if err == nil {
		t.Fatal("401 必须报错，否则账号过期会被伪装成目录正常")
	}
	if v3Hits != 0 {
		t.Fatalf("401 不应切到 v3，实际 v3 被请求 %d 次", v3Hits)
	}
	cache.mu.RLock()
	down := cache.v2Down
	cache.mu.RUnlock()
	if down {
		t.Fatal("401 不得被记成「v2 下线」，否则恢复后也不会再探")
	}
}

// TestFetchCatalogRetriesV2AfterWindow 下线窗口到期后必须重探 v2，成功则切回。
func TestFetchCatalogRetriesV2AfterWindow(t *testing.T) {
	resetCatalogCache()
	v2Broken := true
	var v2Hits int
	srv := testServer(func(w http.ResponseWriter, r *http.Request, n int) {
		switch r.URL.Path {
		case EpCatalog:
			v2Hits++
			if v2Broken {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error_msg":"404 Route Not Found"}`))
				return
			}
			_, _ = w.Write([]byte(v2CatalogBody()))
		case EpCatalogV3:
			_, _ = w.Write([]byte(v3CatalogBody()))
		}
	})
	defer srv.Close()
	c := New()
	c.Base = srv.URL

	if _, err := c.FetchModels(testAuth()); err != nil {
		t.Fatalf("首次(回退): %v", err)
	}
	// 把下线判定拨到过去 + 清目录缓存，模拟 30 分钟过去、v2 已恢复。
	cache.mu.Lock()
	cache.fetched = time.Time{}
	cache.v3Fetched = time.Time{}
	cache.v2RetryAt = time.Now().Add(-time.Second)
	cache.mu.Unlock()
	v2Broken = false

	infos, err := c.FetchModels(testAuth())
	if err != nil {
		t.Fatalf("v2 恢复后: %v", err)
	}
	if v2Hits != 2 {
		t.Fatalf("窗口到期后应重探 v2，期望 2 次，实际 %d 次", v2Hits)
	}
	cache.mu.RLock()
	down := cache.v2Down
	cache.mu.RUnlock()
	if down {
		t.Fatal("v2 恢复后必须清掉下线标记，否则永远走 v3 丢能力字段")
	}
	found := false
	for _, m := range infos {
		if m.ID == "v2-model-a" {
			found = true
		}
	}
	if !found {
		t.Fatalf("恢复后应以 v2 为准，实际 %+v", ids(infos))
	}
}

// TestFetchModelPricingFallsBackToV3 费率面同样必须回退（否则面板倍率整组空白）。
func TestFetchModelPricingFallsBackToV3(t *testing.T) {
	resetCatalogCache()
	srv := testServer(func(w http.ResponseWriter, r *http.Request, n int) {
		switch r.URL.Path {
		case EpCatalog:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error_msg":"404 Route Not Found"}`))
		case EpCatalogV3:
			_, _ = w.Write([]byte(v3CatalogBody()))
		}
	})
	defer srv.Close()
	c := New()
	c.Base = srv.URL

	out, err := c.FetchModelPricing(testAuth())
	if err != nil {
		t.Fatalf("费率回退失败: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("费率表不得为空")
	}
	got := map[string]float64{}
	for _, p := range out {
		got[p.Model] = p.Rate
	}
	if r, ok := got["v3-model-a"]; !ok || r <= 0 {
		t.Fatalf("v3 模型的倍率应被解析，实际 %+v", got)
	}
}

// TestFetchCatalogV3FallbackHitsCacheOnce 回退 v3 后不应为增强再打一次同一端点。
func TestFetchCatalogV3FallbackHitsCacheOnce(t *testing.T) {
	resetCatalogCache()
	var v3Hits int
	srv := testServer(func(w http.ResponseWriter, r *http.Request, n int) {
		switch r.URL.Path {
		case EpCatalog:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error_msg":"404 Route Not Found"}`))
		case EpCatalogV3:
			v3Hits++
			_, _ = w.Write([]byte(v3CatalogBody()))
		}
	})
	defer srv.Close()
	c := New()
	c.Base = srv.URL

	if _, err := c.FetchModels(testAuth()); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if v3Hits != 1 {
		t.Fatalf("回退拿到的 v3 响应应回灌缓存，期望 v3 只被请求 1 次，实际 %d 次", v3Hits)
	}
}

// ids 便于失败时打印。
func ids(infos []provider.ModelInfo) []string {
	out := make([]string, 0, len(infos))
	for _, m := range infos {
		out = append(out, m.ID)
	}
	return out
}

// TestV2RetryIntervalIsSane 下线重探窗口必须是个有限正数（0 会变成每轮都撞）。
func TestV2RetryIntervalIsSane(t *testing.T) {
	if v2RetryInterval <= 0 {
		t.Fatal("v2RetryInterval 必须为正，否则等于不记住下线")
	}
	if v2RetryInterval > time.Hour {
		t.Fatalf("v2RetryInterval=%v 过长，v2 恢复后要等太久才切回", v2RetryInterval)
	}
}

// TestCatalogRequestsSendCLIUserAgent v3 有 UA 门禁：web UA 会拿到 400 code=12403
// "check ua"。这条用例把门禁做成服务端的真实判据——不是断言字符串，而是让
// 假上游像真上游一样拒绝非 CLI UA，从而锁住「两个目录端点都必须带 CLI 形 UA」。
func TestCatalogRequestsSendCLIUserAgent(t *testing.T) {
	resetCatalogCache()
	var seen []string
	srv := testServer(func(w http.ResponseWriter, r *http.Request, n int) {
		ua := r.Header.Get("User-Agent")
		seen = append(seen, r.URL.Path+"|"+ua)
		if !strings.Contains(ua, "CLI/") {
			// 复刻上游门禁：web UA → 400 code=12403
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("{\"code\":12403,\"error_msg\":\"check ua\"}"))
			return
		}
		switch r.URL.Path {
		case EpCatalog:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("{\"error_msg\":\"404 Route Not Found\"}"))
		case EpCatalogV3:
			_, _ = w.Write([]byte(v3CatalogBody()))
		}
	})
	defer srv.Close()
	c := New()
	c.Base = srv.URL

	infos, err := c.FetchModels(testAuth())
	if err != nil {
		t.Fatalf("目录请求必须带 CLI 形 UA（否则 v3 回 400 code=12403），实际 err=%v；已发出：%v", err, seen)
	}
	if len(infos) == 0 {
		t.Fatalf("UA 正确时不应拿空表，已发出：%v", seen)
	}
	for _, s := range seen {
		if !strings.Contains(s, "CLI/") {
			t.Fatalf("每个目录请求都必须带 CLI 形 UA，违规：%s", s)
		}
	}
}

var _ = strings.Contains
