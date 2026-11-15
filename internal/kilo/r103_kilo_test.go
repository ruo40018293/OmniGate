package kilo

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"omnigate/internal/provider"
)

// ---------------------------------------------------------------------------
// 免费集合的判据
// ---------------------------------------------------------------------------

// TestIsFreeRowRequiresUpstreamFlag 锁住「免费」的唯一判据是上游 isFree 布尔。
//
// 反面：不许靠模型名含 free/`:free` 后缀或价格为 0 来推断。上游命名不是契约
// （`stealth/glyph-cluster` 是免费的却不带 :free），而「价格字段缺失」既可能是
// 免费也可能是没给数据 —— 两种误判都会让用户调到付费模型（得 PAID_MODEL_AUTH_REQUIRED）。
func TestIsFreeRowRequiresUpstreamFlag(t *testing.T) {
	cases := []struct {
		name string
		row  listingRow
		want bool
	}{
		{"上游标记且 id 非空", listingRow{ID: "a/b:free", IsFree: true}, true},
		{"上游未标记但名字带 free", listingRow{ID: "a/free-model", IsFree: false}, false},
		{"上游标记但 id 为空", listingRow{ID: "", IsFree: true}, false},
		{"上游标记但 id 全空白", listingRow{ID: "   ", IsFree: true}, false},
	}
	for _, c := range cases {
		if got := IsFreeRow(c.row); got != c.want {
			t.Errorf("%s: IsFreeRow = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestFilterFreeKeepsUpstreamOrderAndDedupes 保证不替上游重排、且按 id 去重。
func TestFilterFreeKeepsUpstreamOrderAndDedupes(t *testing.T) {
	rows := []listingRow{
		{ID: "z/last", IsFree: true},
		{ID: "a/first", IsFree: true},
		{ID: "z/last", IsFree: true},      // 重复
		{ID: "paid/model", IsFree: false}, // 付费必须剔除
		{ID: "m/middle", IsFree: true},
	}
	got := filterFree(rows)
	want := []string{"z/last", "a/first", "m/middle"}
	if len(got) < len(want) {
		t.Fatalf("filterFree 返回 %d 条，至少要 %d 条", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("第 %d 条 = %q, want %q（必须保持上游顺序、按 id 去重）", i, got[i].ID, id)
		}
	}
	for _, m := range got {
		if m.ID == "paid/model" {
			t.Error("付费模型必须被剔除")
		}
	}
}

// ---------------------------------------------------------------------------
// 能力位与容量
// ---------------------------------------------------------------------------

// TestRowToModelInfoDerivesCapabilitiesFromSupportedParameters 锁住能力位只从
// supported_parameters 认，不从模型名猜。
func TestRowToModelInfoDerivesCapabilitiesFromSupportedParameters(t *testing.T) {
	cases := []struct {
		name          string
		params        []string
		modalities    []string
		wantReasoning bool
		wantTools     bool
		wantImages    bool
	}{
		{"reasoning", []string{"reasoning"}, nil, true, false, false},
		{"reasoning_effort", []string{"reasoning_effort"}, nil, true, false, false},
		{"tools 需要 tools+tool_choice 同时存在", []string{"tools"}, nil, false, false, false},
		{"tools+tool_choice", []string{"tools", "tool_choice"}, nil, false, true, false},
		// 实测 6/14 个免费模型能读图，但 supported_parameters 里没有任何 vision 字段，
		// 唯一可靠来源是 architecture.input_modalities。
		{"vision 只在 input_modalities", nil, []string{"text", "image"}, false, false, true},
		{"video 不单独算视觉", nil, []string{"text", "video"}, false, false, false},
		{"audio 不算视觉", nil, []string{"text", "audio"}, false, false, false},
		{"vision 参数声明也认", []string{"vision"}, nil, false, false, true},
		{"image 参数声明也认", []string{"image"}, nil, false, false, true},
		{"image_url 参数声明也认", []string{"image_url"}, nil, false, false, true},
		{"大小写与空白不敏感", []string{"  TOOLS ", "Tool_Choice"}, nil, false, true, false},
		{"模态大小写与空白不敏感", nil, []string{" TEXT ", " Image "}, false, false, true},
		{"全无", []string{"temperature", "top_p"}, []string{"text"}, false, false, false},
	}
	for _, c := range cases {
		mi := rowToModelInfo(listingRow{
			ID: "x/y", IsFree: true,
			SupportedParameters: c.params,
			Architecture:        architecture{InputModalities: c.modalities},
		})
		if mi.SupportsReasoning != c.wantReasoning {
			t.Errorf("%s: SupportsReasoning = %v, want %v", c.name, mi.SupportsReasoning, c.wantReasoning)
		}
		if mi.SupportsTools != c.wantTools {
			t.Errorf("%s: SupportsTools = %v, want %v", c.name, mi.SupportsTools, c.wantTools)
		}
		if mi.SupportsImages != c.wantImages {
			t.Errorf("%s: SupportsImages = %v, want %v", c.name, mi.SupportsImages, c.wantImages)
		}
	}

	// 名字里带 reasoning 但上游没声明参数 ⇒ 不得标成支持思考。
	mi := rowToModelInfo(listingRow{ID: "nvidia/nemotron-reasoning:free", IsFree: true})
	if mi.SupportsReasoning {
		t.Error("模型名带 reasoning 不代表支持 reasoning：能力位必须来自 supported_parameters")
	}
}

// TestRowToModelInfoContextFallbacks 容量三级回退，且 ContextFromAPI 只在
// 「context 与 max_output 都来自上游」时为 true。
func TestRowToModelInfoContextFallbacks(t *testing.T) {
	// 全给：官方值。
	mi := rowToModelInfo(listingRow{ID: "a", IsFree: true, ContextLength: 512000, MaxOutput: 65536})
	if mi.ContextWindow != 512000 || mi.MaxTokens != 65536 || !mi.ContextFromAPI {
		t.Errorf("全给时 = (%d,%d,fromAPI=%v), want (512000,65536,true)",
			mi.ContextWindow, mi.MaxTokens, mi.ContextFromAPI)
	}

	// 顶层缺 context，退到 top_provider：仍是上游数据。
	mi = rowToModelInfo(listingRow{
		ID: "b", IsFree: true, MaxOutput: 32000,
		TopProvider: topProviderInfo{ContextLength: 200000},
	})
	if mi.ContextWindow != 200000 || !mi.ContextFromAPI {
		t.Errorf("退到 top_provider 时 = (%d,fromAPI=%v), want (200000,true)", mi.ContextWindow, mi.ContextFromAPI)
	}

	// 只给 max_output、两处都没 context ⇒ 上下文是本地默认值，不得标 fromAPI。
	mi = rowToModelInfo(listingRow{ID: "c", IsFree: true, MaxOutput: 32000})
	if mi.ContextWindow != defaultContextWindow {
		t.Errorf("缺 context 时 = %d, want 默认 %d", mi.ContextWindow, defaultContextWindow)
	}
	if mi.ContextFromAPI {
		t.Error("只给了 max_output 没给 context_length：上下文是本地估值，ContextFromAPI 必须为 false")
	}

	// 全缺：两个默认值，且不是官方值。
	mi = rowToModelInfo(listingRow{ID: "d", IsFree: true})
	if mi.ContextWindow != defaultContextWindow || mi.MaxTokens != defaultMaxTokens {
		t.Errorf("全缺时 = (%d,%d), want (%d,%d)",
			mi.ContextWindow, mi.MaxTokens, defaultContextWindow, defaultMaxTokens)
	}
	if mi.ContextFromAPI {
		t.Error("全缺时 ContextFromAPI 必须为 false")
	}
}

// TestStaticFreeHasNoFakeAPIProvenance 静态兜底只有一条，且必须自认是估值。
func TestStaticFreeHasNoFakeAPIProvenance(t *testing.T) {
	got := staticFree()
	if len(got) != 1 {
		t.Fatalf("staticFree 应只有 1 条（唯一有公开出处的 kilo-auto/free），实际 %d 条", len(got))
	}
	if got[0].ID != "kilo-auto/free" {
		t.Errorf("静态条目 = %q, want kilo-auto/free", got[0].ID)
	}
	if got[0].ContextFromAPI {
		t.Error("静态兜底的容量是估值，ContextFromAPI 必须为 false（否则面板会当官方值展示）")
	}
	// StaticModels 必须与 staticFree 同源，不许另立一份。
	sm := StaticModels()
	if len(sm) != 1 || sm[0].ID != got[0].ID {
		t.Errorf("StaticModels 与 staticFree 不同源: %+v", sm)
	}
	if sm[0].ContextFromAPI != got[0].ContextFromAPI {
		t.Error("StaticModels 与 staticFree 的 ContextFromAPI 必须一致")
	}
}

// TestFilterFreeUpstreamCapabilitiesWinOverStatic 上游与静态同 id 时，
// 以上游为基底，静态只补上游没声明的能力位。
func TestFilterFreeUpstreamCapabilitiesWinOverStatic(t *testing.T) {
	got := filterFree([]listingRow{{
		ID: "kilo-auto/free", IsFree: true,
		ContextLength: 999000, MaxOutput: 111000,
		SupportedParameters: []string{"tools", "tool_choice"},
	}})
	if len(got) != 1 {
		t.Fatalf("同 id 必须去重成 1 条，实际 %d 条", len(got))
	}
	if got[0].ContextWindow != 999000 || got[0].MaxTokens != 111000 {
		t.Errorf("容量必须以上游为准 = (%d,%d), want (999000,111000)", got[0].ContextWindow, got[0].MaxTokens)
	}
	if !got[0].SupportsTools {
		t.Error("上游声明的 tools 不得被静态兜底抹掉")
	}
}

// TestFilterFreeEmptyFallsBackToStatic 上游一条免费都没有时给静态兜底。
func TestFilterFreeEmptyFallsBackToStatic(t *testing.T) {
	got := filterFree(nil)
	if len(got) != 1 || got[0].ID != "kilo-auto/free" {
		t.Fatalf("空清单必须回退静态兜底，实际 %+v", got)
	}
}

// ---------------------------------------------------------------------------
// 清单拉取失败的行为
// ---------------------------------------------------------------------------

// TestFetchModelsFallsBackToStaticWithoutError 锁住「清单失败不是账号故障」。
//
// 反面：返回 error 会让 handler 把它当上游故障，而这条渠道挂的是一个**无法替换**
// 的虚拟账号（SingleAccount），一次 500 就足以让整条渠道在面板上消失。
func TestFetchModelsFallsBackToStaticWithoutError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"boom"}`)
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	models, err := c.FetchModels(AnonymousAuth())
	if err != nil {
		t.Fatalf("清单失败不得返回 error（会让整条渠道下线），实际: %v", err)
	}
	if len(models) != 1 || models[0].ID != "kilo-auto/free" {
		t.Fatalf("清单失败必须回退静态兜底，实际 %+v", models)
	}
}

// TestFetchModelsEmptyListingFallsBackToStatic 上游返回 200 但 data 为空。
func TestFetchModelsEmptyListingFallsBackToStatic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	models, err := c.FetchModels(AnonymousAuth())
	if err != nil {
		t.Fatalf("空清单不得返回 error，实际: %v", err)
	}
	if len(models) != 1 || models[0].ID != "kilo-auto/free" {
		t.Fatalf("空清单必须回退静态兜底，实际 %+v", models)
	}
}

// TestFetchModelsUsesListingWhenAvailable 正常路径：清单可用时用清单。
func TestFetchModelsUsesListingWhenAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpModels {
			t.Errorf("请求路径 = %q, want %q", r.URL.Path, EpModels)
		}
		if got := r.Header.Get("User-Agent"); got != userAgent {
			t.Errorf("User-Agent = %q, want %q", got, userAgent)
		}
		// 字段名按上游真实形态写：isFree 是驼峰，输出上限在
		// top_provider.max_completion_tokens（顶层没有 max_output）。
		fmt.Fprint(w, `{"data":[
			{"id":"nvidia/nemotron:free","name":"Nemotron","isFree":true,
			 "context_length":1000000,
			 "top_provider":{"context_length":1000000,"max_completion_tokens":32000},
			 "supported_parameters":["reasoning","tools","tool_choice"]},
			{"id":"paid/model","isFree":false,"context_length":1}
		]}`)
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	models, err := c.FetchModels(AnonymousAuth())
	if err != nil {
		t.Fatalf("FetchModels 失败: %v", err)
	}
	// 上游那条排在前，静态兜底 kilo-auto/free 补在末尾（上游这份清单没给它是刻意的：
	// 静态条目只在上游没给时才补，且绝不覆盖上游声明）。
	if len(models) != 2 {
		t.Fatalf("应留下 1 条上游免费 + 1 条静态兜底，实际 %d: %+v", len(models), models)
	}
	if models[0].ID != "nvidia/nemotron:free" || !models[0].SupportsReasoning || !models[0].SupportsTools {
		t.Errorf("清单项解析不完整: %+v", models[0])
	}
	if !models[0].ContextFromAPI {
		t.Error("清单给了 context_length 与 top_provider.max_completion_tokens，ContextFromAPI 应为 true")
	}
	if models[0].MaxTokens != 32000 {
		t.Errorf("MaxTokens = %d, want 32000（必须从 top_provider.max_completion_tokens 取到）", models[0].MaxTokens)
	}
}

// TestParseRealListingShape 用**上游真实响应形状**回归：字段名一个都不能猜错。
//
// 这段 JSON 取自 2026-11-15 https://api.kilo.ai/api/gateway/models 的原文片段
// （裁掉无关字段）。它锁住三条实测结论：
//  1. 免费标记是驼峰 `isFree`，不是 `is_free`；
//  2. 输出上限在 `top_provider.max_completion_tokens`，顶层没有 `max_output`；
//  3. 视觉能力只在 `architecture.input_modalities` 里，`supported_parameters` 无 vision。
func TestParseRealListingShape(t *testing.T) {
	const raw = `{"data":[
		{"id":"nvidia/nemotron-3.5-super-vl:free","name":"Nemotron 3.5 Super VL",
		 "architecture":{"input_modalities":["text","image","video"],"output_modalities":["text"]},
		 "top_provider":{"is_moderated":false,"context_length":1000000,"max_completion_tokens":131072},
		 "context_length":1000000,
		 "supported_parameters":["include_reasoning","max_tokens","reasoning","response_format","seed","structured_outputs","temperature","tool_choice","tools","top_p"],
		 "isFree":true},
		{"id":"kilo-auto/free","name":"Kilo Auto (free)",
		 "architecture":{"input_modalities":["text"]},
		 "top_provider":{"context_length":256000,"max_completion_tokens":32768},
		 "context_length":256000,
		 "supported_parameters":["max_tokens","temperature","tools","reasoning","include_reasoning"],
		 "isFree":true},
		{"id":"anthropic/claude-paid","name":"Paid",
		 "top_provider":{"context_length":200000,"max_completion_tokens":8192},
		 "context_length":200000,"supported_parameters":["tools"],"isFree":false}
	]}`
	var wrap struct {
		Data []listingRow `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &wrap); err != nil {
		t.Fatalf("解析真实形状失败: %v", err)
	}
	if len(wrap.Data) != 3 {
		t.Fatalf("应解析出 3 行，实际 %d", len(wrap.Data))
	}
	if !wrap.Data[0].IsFree || !wrap.Data[1].IsFree {
		t.Error("isFree 没有被正确解析（字段名是驼峰 isFree）")
	}
	if wrap.Data[2].IsFree {
		t.Error("付费条目不得被当成免费")
	}
	if wrap.Data[0].TopProvider.MaxCompletionTokens != 131072 {
		t.Errorf("max_completion_tokens = %d, want 131072", wrap.Data[0].TopProvider.MaxCompletionTokens)
	}

	got := filterFree(wrap.Data)
	if len(got) != 2 {
		t.Fatalf("应留下 2 条免费，实际 %d: %+v", len(got), got)
	}
	vl := got[0]
	if vl.ID != "nvidia/nemotron-3.5-super-vl:free" {
		t.Fatalf("首条 = %q", vl.ID)
	}
	if !vl.SupportsImages {
		t.Error("input_modalities 含 image 的模型必须标成支持图片（supported_parameters 里没有 vision 字段）")
	}
	if !vl.SupportsTools || !vl.SupportsReasoning {
		t.Errorf("能力位解析不全: tools=%v reasoning=%v", vl.SupportsTools, vl.SupportsReasoning)
	}
	if vl.MaxTokens != 131072 {
		t.Errorf("MaxTokens = %d, want 131072（来自 top_provider.max_completion_tokens）", vl.MaxTokens)
	}
	if !vl.ContextFromAPI {
		t.Error("context_length 与 max_completion_tokens 都是上游声明，ContextFromAPI 应为 true")
	}
	// kilo-auto/free 只有 tools 没有 tool_choice ⇒ 不算支持工具调用；有 reasoning ⇒ 支持思考。
	auto := got[1]
	if auto.ID != "kilo-auto/free" {
		t.Fatalf("第二条 = %q", auto.ID)
	}
	if auto.SupportsTools {
		t.Error("只有 tools 没有 tool_choice 不得标成支持工具调用（发了 tool_calls 上游会拒）")
	}
	if !auto.SupportsReasoning {
		t.Error("有 reasoning 参数应标成支持思考")
	}
	if auto.SupportsImages {
		t.Error("input_modalities 只有 text，不得标成支持图片")
	}
}

// ---------------------------------------------------------------------------
// 定价
// ---------------------------------------------------------------------------

// TestFetchModelPricingIsExplicitZero 免费池价格恒 0 且必须是**显式** 0（R96）：
// Explicit=false 会让面板显示「未知倍率」，Explicit=true 显示「免费」。
func TestFetchModelPricingIsExplicitZero(t *testing.T) {
	c := NewWithBase("http://127.0.0.1:1") // 连不上 ⇒ 走静态兜底，正好测兜底路径的定价
	prices, err := c.FetchModelPricing(AnonymousAuth())
	if err != nil {
		t.Fatalf("FetchModelPricing 失败: %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("定价列表不得为空")
	}
	for _, p := range prices {
		if p.Rate != 0 {
			t.Errorf("%s 的倍率 = %v, want 0", p.Model, p.Rate)
		}
		if p.Explicit == nil || !*p.Explicit {
			t.Errorf("%s 的 Explicit 必须为 true（否则面板显示「未知倍率」而不是「免费」）", p.Model)
		}
		if p.Channel != Kind {
			t.Errorf("%s 的 Channel = %q, want %q", p.Model, p.Channel, Kind)
		}
	}
}

// ---------------------------------------------------------------------------
// Classify
// ---------------------------------------------------------------------------

// TestClassifyNeverDisablesTheVirtualAccount 最关键的一条：绝不能返回 ErrSessionDead。
//
// ErrSessionDead 会让 handler 调 pool.Disable 永久禁用账号；而这条渠道的账号是
// 虚拟的、无法重新登录也无法替换（也不许用户删除），禁掉就是整条渠道永久下线。
func TestClassifyNeverDisablesTheVirtualAccount(t *testing.T) {
	c := New()
	bodies := []string{
		`{"error":"unauthorized"}`,
		`{"error":{"message":"invalid api key"}}`,
		`{"error":{"message":"session expired"}}`,
		`{"error":"token invalid"}`,
		``,
	}
	statuses := []int{400, 401, 403, 404, 422, 429, 500, 502, 503}
	for _, st := range statuses {
		for _, b := range bodies {
			if got := c.Classify(st, b); got == provider.ErrSessionDead {
				t.Errorf("Classify(%d, %q) = ErrSessionDead —— 会永久禁用无法恢复的虚拟账号", st, b)
			}
		}
	}
}

// TestClassifyTable 逐状态断言分类（与 handler 的惩罚分支对齐）。
func TestClassifyTable(t *testing.T) {
	c := New()
	cases := []struct {
		status int
		body   string
		want   provider.ErrKind
	}{
		{429, `{"error":"rate limited"}`, provider.ErrSoftRate},
		{500, ``, provider.ErrServer},
		{502, `bad gateway`, provider.ErrServer},
		{503, `unavailable`, provider.ErrServer},
		// 付费模型未授权 ⇒ 请求级错误（不轮转、不罚号），客户端换模型即可。
		{400, `{"error":"paid_model_auth_required"}`, provider.ErrClient},
		{400, `{"error":"PAID_MODEL_AUTH_REQUIRED"}`, provider.ErrClient},
		{400, `{"error":"moderation"}`, provider.ErrClient},
		{400, `{"error":"flagged"}`, provider.ErrClient},
		{400, `{"error":"filtered"}`, provider.ErrClient},
		// 其余 4xx 一律透传：免费池没有账号可罚，原文给客户端才有信息量。
		{400, `{"error":"some other problem"}`, provider.ErrPassthrough},
		{404, `{"error":"model not found"}`, provider.ErrPassthrough},
		{422, ``, provider.ErrPassthrough},
		{200, ``, provider.ErrNone},
	}
	for _, tc := range cases {
		if got := c.Classify(tc.status, tc.body); got != tc.want {
			t.Errorf("Classify(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
		}
	}
}

// TestClassifyPassthroughDoesNotRotate 锁住封装语义：透传类错误不可轮转，
// 请求级错误归 ErrClient（语义正确：客户端要了一个需要授权的模型）。
//
// 注意 ErrClient **在** provider.Rotatable() 白名单里 —— 这是全局口径，不是本渠道的
// 漏洞：kilo 装配时带 SingleAccount=true，handler 在进 switch 之前就走
// 「单账号渠道不罚账号，原文透传」分支，根本走不到轮转逻辑。
// 这里断言的是两点：①透传类确实不可轮转；②付费模型未授权不得被归成
// 任何账号级惩罚（尤其绝不是 ErrSessionDead）。
func TestClassifyPassthroughDoesNotRotate(t *testing.T) {
	c := New()
	if k := c.Classify(404, `{"error":"model not found"}`); k.Rotatable() {
		t.Errorf("%v 不该可轮转（虚拟账号无号可换）", k)
	}
	k := c.Classify(400, `{"error":"paid_model_auth_required"}`)
	if k != provider.ErrClient {
		t.Errorf("付费模型未授权应归 ErrClient，实际 %v", k)
	}
	for _, bad := range []provider.ErrKind{provider.ErrSessionDead, provider.ErrHardCredit} {
		if k == bad {
			t.Errorf("付费模型未授权不得归 %v（账号级惩罚会伤到不可恢复的虚拟账号）", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// 虚拟账号与占位实现
// ---------------------------------------------------------------------------

// TestAnonymousAuthIsFarFuture 到期时间必须是远期哨兵。
//
// 反面：ExpiresAt 为 0 时 auth.NeedsRefreshLocked 恒返 true，于是每次挑号都会
// 触发一次 RefreshToken —— 免费池没有 refresh 端点，这会变成永动机式空转。
func TestAnonymousAuthIsFarFuture(t *testing.T) {
	a := AnonymousAuth()
	if a.ExpiresAt != noExpiry {
		t.Errorf("ExpiresAt = %d, want 远期哨兵 %d", a.ExpiresAt, noExpiry)
	}
	if time.Unix(a.ExpiresAt, 0).Before(time.Now()) {
		t.Error("远期哨兵必须晚于现在")
	}
	if a.AccessToken == "" {
		t.Error("占位 token 不得为空（auth 需要一个非空凭据才能进入池）")
	}
	if a.UID != AnonymousUID {
		t.Errorf("UID = %q, want %q", a.UID, AnonymousUID)
	}
	if a.Kind != Kind {
		t.Errorf("Kind = %q, want %q", a.Kind, Kind)
	}
	if a.NeedsRefresh(10 * time.Minute) {
		t.Error("远期哨兵不得触发刷新（免费池无 refresh 端点）")
	}
}

// TestNoopAccountEndpoints 确认无副作用端点不谎报成功。
func TestNoopAccountEndpoints(t *testing.T) {
	c := New()
	a := AnonymousAuth()
	if err := c.RefreshToken(a); err != nil {
		t.Errorf("RefreshToken 应为无操作: %v", err)
	}
	if n, err := c.UserResource(a); n != 0 || err != nil {
		t.Errorf("UserResource = (%d,%v), want (0,nil)", n, err)
	}
	if n, items, err := c.UserResourceDetail(a); n != 0 || items != nil || err != nil {
		t.Errorf("UserResourceDetail = (%d,%v,%v), want (0,nil,nil)", n, items, err)
	}
	if err := c.DailyCheckin(a); err == nil {
		t.Error("DailyCheckin 必须报错（免费池无签到），静默成功会让面板显示「签到成功」")
	}
}

// ---------------------------------------------------------------------------
// ChatStream
// ---------------------------------------------------------------------------

// TestChatStreamForcesStreamTrue 非流式请求也必须被改写成流式。
//
// 理由：本渠道的 Stream/Aggregate 两条出口都只按 SSE 帧解析，只有流式路径被验证过。
func TestChatStreamForcesStreamTrue(t *testing.T) {
	var gotBody map[string]any
	var gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpChat {
			t.Errorf("路径 = %q, want %q", r.URL.Path, EpChat)
		}
		gotAccept = r.Header.Get("Accept")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {}\n\n")
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	rc, status, raw, err := c.ChatStream(AnonymousAuth(), []byte(`{"model":"kilo-auto/free","stream":false}`))
	if err != nil || status != 200 {
		t.Fatalf("ChatStream = (%v,%d,%s,%v)", rc, status, raw, err)
	}
	rc.Close()
	if v, ok := gotBody["stream"]; !ok || v != true {
		t.Errorf("出站 stream = %v (存在=%v), 必须强制为 true", v, ok)
	}
	if !strings.Contains(gotAccept, "text/event-stream") {
		t.Errorf("Accept = %q, 必须包含 text/event-stream", gotAccept)
	}
}

// TestChatStreamReturnsUpstreamStatusBody 上游 4xx/5xx 必须把状态码与原文交回。
func TestChatStreamReturnsUpstreamStatusBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":"slow down"}`)
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	rc, status, raw, err := c.ChatStream(AnonymousAuth(), []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("上游 429 不应是传输层错误: %v", err)
	}
	if rc != nil {
		t.Error("非 200 时不得返回可读流")
	}
	if status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", status)
	}
	if !strings.Contains(string(raw), "slow down") {
		t.Errorf("必须把上游原文交回 handler 以便透传，实际 %q", string(raw))
	}
}

// TestChatStreamInvalidJSONIsLocalBadRequest 本地 JSON 错误走 400 且不换号（R88 同型）。
func TestChatStreamInvalidJSONIsLocalBadRequest(t *testing.T) {
	c := NewWithBase("http://127.0.0.1:1") // 不该被访问
	rc, status, raw, err := c.ChatStream(AnonymousAuth(), []byte(`{not json`))
	if err != nil {
		t.Fatalf("本地 JSON 错误不应是传输层错误: %v", err)
	}
	if rc != nil || status != http.StatusBadRequest {
		t.Fatalf("= (%v,%d), want (nil,400)", rc, status)
	}
	if !strings.Contains(string(raw), "invalid JSON") {
		t.Errorf("错误体应说明是本地 JSON 问题，实际 %q", string(raw))
	}
}

// ---------------------------------------------------------------------------
// 流内模型名改写
// ---------------------------------------------------------------------------

// TestModelRewriterRewritesEachLine 裸模型 id 必须逐行改写成带渠道前缀的名字。
func TestModelRewriterRewritesEachLine(t *testing.T) {
	src := `data: {"id":"x","model":"kilo-auto/free","choices":[]}` + "\n" +
		`data: {"id":"x","model":"other/model","choices":[]}` + "\n"
	r := newModelRewriter(strings.NewReader(src), "kilo/kilo-auto/free")
	out, err := io.ReadAll(r)
	if err != nil && err != io.EOF {
		t.Fatalf("读取失败: %v", err)
	}
	got := string(out)
	if strings.Count(got, `"model":"kilo/kilo-auto/free"`) != 1 {
		t.Errorf("出站必须以改写后的名字为主，实际:\n%s", got)
	}
	if !strings.Contains(got, `"model":"kilo/kilo-auto/free"`) {
		t.Errorf("模型名未改写:\n%s", got)
	}
	if strings.Count(got, "\n") != 2 {
		t.Errorf("行数必须保持 2（按行处理不得吞换行），实际:\n%s", got)
	}
}

// TestModelRewriterDoesNotDoublePrefix 已带渠道前缀的帧必须原样保留。
//
// 反面：上游偶尔回显我们发过去的模型名，再拼一次会变成 kilo/kilo/xxx，
// 客户端当场认不出自己的模型。
func TestModelRewriterDoesNotDoublePrefix(t *testing.T) {
	src := `data: {"model":"kilo/kilo-auto/free"}` + "\n"
	r := newModelRewriter(strings.NewReader(src), "kilo/kilo-auto/free")
	out, _ := io.ReadAll(r)
	if strings.Contains(string(out), "kilo/kilo/kilo") {
		t.Errorf("出现三重前缀:\n%s", string(out))
	}
	if !strings.Contains(string(out), `"model":"kilo/kilo-auto/free"`) {
		t.Errorf("已带前缀的必须原样保留:\n%s", string(out))
	}
}

// TestModelRewriterStreamsIncrementally 锁住「不攒整条流」。
//
// 反面：攒到 EOF 再替换会让流式退化成一次性返回（R35/R56 明确禁止的退化）。
// 做法：源是一个「读一次就阻塞直到后续写入」的管道 —— 若 rewriter 攒流，
// 第一次 Read 会永远拿不到数据，测试超时失败。
func TestModelRewriterStreamsIncrementally(t *testing.T) {
	pr, pw := io.Pipe()
	r := newModelRewriter(pr, "kilo/m")
	go func() {
		fmt.Fprint(pw, "data: {\"model\":\"m\"}\n")
		// 只写一行，绝不关闭：攒流的实现会在这里卡死。
	}()
	defer pw.Close()

	done := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, err := r.Read(buf)
		if err != nil {
			done <- "err:" + err.Error()
			return
		}
		done <- string(buf[:n])
	}()
	select {
	case got := <-done:
		if !strings.Contains(got, `"model":"kilo/m"`) {
			t.Errorf("首行未改写就吐给了客户端: %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("第一行到达后 3 秒还没吐给客户端 —— 说明实现攒整条流才输出，流式已退化成一次性")
	}
}

// TestModelRewriterEmptyModelPassesThrough 未指定模型时原样透传（Aggregate 用）。
func TestModelRewriterEmptyModelPassesThrough(t *testing.T) {
	src := `{"model":"whatever"}` + "\n"
	r := newModelRewriter(strings.NewReader(src), "")
	out, _ := io.ReadAll(r)
	if string(out) != src {
		t.Errorf("空模型名时必须原样透传，实际 %q，want %q", string(out), src)
	}
}

// ---------------------------------------------------------------------------
// 常量自检
// ---------------------------------------------------------------------------

// TestConstantsAreSane 锁住几条最容易写错的常量。
func TestConstantsAreSane(t *testing.T) {
	if noExpiry <= time.Now().Unix() {
		t.Errorf("noExpiry = %d 必须是远期值，否则每轮挑号都会触发刷新", noExpiry)
	}
	if DefaultBase != "https://api.kilo.ai/api/gateway" {
		t.Errorf("DefaultBase = %q", DefaultBase)
	}
	if !strings.HasPrefix(EpChat, "/") || !strings.HasPrefix(EpModels, "/") {
		t.Errorf("端点必须以 / 开头: chat=%q models=%q", EpChat, EpModels)
	}
	if AnonymousUID == "" || userAgent == "" {
		t.Error("AnonymousUID / userAgent 不得为空")
	}
	if requestTimeout <= 0 || listTimeout <= 0 {
		t.Errorf("超时必须为正: request=%v list=%v", requestTimeout, listTimeout)
	}
	if listTimeout > requestTimeout {
		t.Error("清单超时不该比总超时还长")
	}
}

// TestKindMatchesProviderConstant 渠道标识必须与 provider.Kilo 一致（否则统计与面板对不上）。
func TestKindMatchesProviderConstant(t *testing.T) {
	if string(provider.Kilo) != Kind {
		t.Errorf("kilo.Kind = %q 与 provider.Kilo = %q 不一致", Kind, provider.Kilo)
	}
}
