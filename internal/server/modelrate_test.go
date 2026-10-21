package server

// modelrate_test.go 带倍率模型 id 的下发与路由识别。
//
// 核心不变量（下发与识别必须成对）：
//
//	/v1/models 下发的 id  →  用户复制粘贴回客户端  →  路由仍能找到同一个上游模型
//
// 只测一侧都会漏 bug：只测下发，用户粘回来就 400；只测识别，用户永远看不到倍率。

import (
	"strings"
	"testing"

	"omnigate/internal/auth"
	"omnigate/internal/pool"
	"omnigate/internal/provider"
)

// rateHandler 造一个带倍率查询钩子的 handler（固定倍率表）。
//
// Upstream 用 rotatingUpstream（同包已有）：runtimeForModel 要求 Runtime 三件套
// 齐备（Pool/Upstream 非 nil），本测试只关心路由解析，用现成 mock 即可。
func rateHandler(rates map[provider.Kind]map[string]float64) *Handler {
	p := pool.New("")
	p.Add(&auth.Auth{Kind: "workbuddy", UID: "u1", RefreshToken: "rt"})
	return &Handler{cfg: Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: p, Upstream: &rotatingUpstream{}},
		},
		ModelRate: func(k provider.Kind, model string) (float64, bool) {
			r, ok := rates[k][model]
			return r, ok
		},
	}}
}

// TestModelEntryAppendsRate 有倍率的模型 → id 带 " (0.06x)"，upstream_id 保持裸名。
func TestModelEntryAppendsRate(t *testing.T) {
	h := rateHandler(map[provider.Kind]map[string]float64{
		provider.WorkBuddy: {"hy3": 0.06, "kimi-k3": 1.83},
	})

	e := h.buildModelEntry(provider.WorkBuddy, provider.ModelInfo{ID: "hy3"})
	if e["id"] != "workbuddy/hy3 (0.06x)" {
		t.Fatalf("id=%v，应为 workbuddy/hy3 (0.06x)", e["id"])
	}
	// 程序化消费方需要裸名
	if e["upstream_id"] != "workbuddy/hy3" {
		t.Errorf("upstream_id=%v，应为 workbuddy/hy3", e["upstream_id"])
	}
	// 倍率数字本身也单列一份，省得消费方去解析字符串
	if e["credit_rate"] != 0.06 {
		t.Errorf("credit_rate=%v，应为 0.06", e["credit_rate"])
	}

	// 无倍率的模型 → 保持裸 id，绝不用 0 冒充
	e2 := h.buildModelEntry(provider.WorkBuddy, provider.ModelInfo{ID: "no-price-model"})
	if e2["id"] != "workbuddy/no-price-model" {
		t.Errorf("无倍率模型 id=%v，应保持裸名", e2["id"])
	}
	if _, has := e2["credit_rate"]; has {
		t.Error("无倍率时不应输出 credit_rate")
	}
}

// TestModelRateNilHookKeepsBareIDs 未注入钩子（老调用方/测试）→ 行为不变。
func TestModelRateNilHookKeepsBareIDs(t *testing.T) {
	h := &Handler{}
	e := h.buildModelEntry(provider.WorkBuddy, provider.ModelInfo{ID: "hy3"})
	if e["id"] != "workbuddy/hy3" {
		t.Fatalf("id=%v，未注入钩子时应为裸名", e["id"])
	}
}

// TestRuntimeForModelAcceptsSuffixedID 下发的名字必须能被路由识别。
//
// 这条是整个特性的命门：用户把客户端下拉框里的 `workbuddy/hy3 (0.06x)`
// 复制到配置里，必须能正常请求。
func TestRuntimeForModelAcceptsSuffixedID(t *testing.T) {
	h := rateHandler(map[provider.Kind]map[string]float64{
		provider.WorkBuddy: {"hy3": 0.06},
	})

	cases := []struct {
		in       string
		wantKind provider.Kind
		wantMdl  string
	}{
		{"workbuddy/hy3 (0.06x)", provider.WorkBuddy, "hy3"}, // 完整下发形态
		{"workbuddy/hy3(0.06x)", provider.WorkBuddy, "hy3"},  // 丢空格
		{"workbuddy/hy3 (0.06X)", provider.WorkBuddy, "hy3"}, // 大写 X
		{"  workbuddy/hy3 (1.83x)  ", provider.WorkBuddy, "hy3"},
		{"workbuddy/hy3", provider.WorkBuddy, "hy3"}, // 裸名（老配置）
	}
	for _, c := range cases {
		gotRT, gotMdl, err := h.runtimeForModel(c.in)
		if err != nil {
			t.Errorf("runtimeForModel(%q) 报错：%v（带倍率的名字必须能路由）", c.in, err)
			continue
		}
		if gotRT.Kind != c.wantKind || gotMdl != c.wantMdl {
			t.Errorf("runtimeForModel(%q)=(%v,%q)，want (%v,%q)", c.in, gotRT.Kind, gotMdl, c.wantKind, c.wantMdl)
		}
	}
}

// TestRuntimeForModelRejectsGarbage 结构非法的名字仍应报错。
//
// 注意：`workbuddy/hy3 (abcx)` 这类**不**算非法 —— 它没有「数字x」后缀，
// 会被整体当作模型名 `hy3 (abcx)` 交给上游，由上游回 404。这才是正确的分层：
// 路由层只认「合法前缀 + 非空模型名」，不替上游猜模型是否存在。
func TestRuntimeForModelRejectsGarbage(t *testing.T) {
	h := rateHandler(nil)
	for _, in := range []string{
		"",           // 空
		"hy3",        // 无渠道前缀
		"workbuddy/", // 无模型名
		"(0.06x)",    // 纯后缀，无 base
	} {
		if _, _, err := h.runtimeForModel(in); err == nil {
			t.Errorf("runtimeForModel(%q) 应报错", in)
		}
	}

	// 非倍率形态的括号属于模型名的一部分，原样透传给上游（不被误剥）
	for _, c := range []struct{ in, want string }{
		{"workbuddy/hy3 (abcx)", "hy3 (abcx)"}, // 括号里不是 数字x
		{"workbuddy/hy3 (0.06x", "hy3 (0.06x"}, // 缺右括号 → 不构成后缀
	} {
		_, mdl, err := h.runtimeForModel(c.in)
		if err != nil {
			t.Errorf("runtimeForModel(%q) 不该报错：%v", c.in, err)
			continue
		}
		if mdl != c.want {
			t.Errorf("runtimeForModel(%q) 模型名=%q，want %q", c.in, mdl, c.want)
		}
	}
}

// TestRuntimeForModelKeepsParenModelNames 模型名本身带括号且不含倍率 → 不得误剥。
func TestRuntimeForModelKeepsParenModelNames(t *testing.T) {
	h := rateHandler(nil)
	_, mdl, err := h.runtimeForModel("workbuddy/model (preview)")
	if err != nil {
		t.Fatalf("报错：%v", err)
	}
	if mdl != "model (preview)" {
		t.Fatalf("模型名=%q，应原样保留括号部分", mdl)
	}
}

// TestModelListSuffixedIDsEndToEnd modelList 全链路：条目的 id 与路由必须自洽。
//
// 直接调 buildModelEntry 而非 modelList：后者会先走 fetchRuntimeModels
// （真实网络 + 动态缓存），在测试里不可控、且可能返回上游的部分模型列表。
// 这里要验的是「条目 id ↔ 路由」的配对关系，与列表从哪来无关。
func TestModelListSuffixedIDsEndToEnd(t *testing.T) {
	h := rateHandler(map[provider.Kind]map[string]float64{
		provider.WorkBuddy: {"hy3": 0.06},
	})

	var suffixed string
	for _, mi := range []provider.ModelInfo{{ID: "hy3"}, {ID: "no-price"}} {
		if id, _ := h.buildModelEntry(provider.WorkBuddy, mi)["id"].(string); strings.HasSuffix(id, " (0.06x)") {
			suffixed = id
		}
	}
	if suffixed == "" {
		t.Fatal("未产出带倍率条目")
	}
	// 产出的 id 必须能直接回喂给路由
	rt, mdl, err := h.runtimeForModel(suffixed)
	if err != nil || rt.Kind != provider.WorkBuddy || mdl != "hy3" {
		t.Fatalf("下发 id 无法回路由：%q → (%v,%q,%v)", suffixed, rt, mdl, err)
	}
}
