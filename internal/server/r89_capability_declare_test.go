// r89_capability_declare_test.go 锁住 R89 的声明契约：
// /v1/models 必须把「上游已经声明过」的思考档位 / 上下文档位原样告诉客户端，
// 同时**不许编造**——上游没声明的字段一个都不能出现。
//
// 背景：客户端能不能把「思考等级」「上下文大小」渲染成下拉框，唯一数据源就是这里。
// 之前 provider.ModelInfo 已经装了档位表，却在出口处丢掉，于是模型能接受
// reasoning_effort，用户却看不到有哪些档可选。
package server

import (
	"reflect"
	"testing"

	"omnigate/internal/provider"
)

// TestBuildModelEntryDeclaresUpstreamCapabilities 上游声明了什么就输出什么。
func TestBuildModelEntryDeclaresUpstreamCapabilities(t *testing.T) {
	canDisable := true
	mi := provider.ModelInfo{
		ID:                     "minimax/MiniMax-M3.1-Flash",
		ContextWindow:          1000000,
		MaxTokens:              128000,
		ContextFromAPI:         true,
		SupportsReasoning:      true,
		ReasoningEfforts:       []string{"low", "medium", "high"},
		DefaultReasoningEffort: "medium",
		CanDisableThinking:     &canDisable,
		ContextWindows:         []int64{200000, 1000000},
	}
	entry := (&Handler{}).buildModelEntry(provider.Minimax, mi)

	got, ok := entry["reasoning_efforts"].([]string)
	if !ok || !reflect.DeepEqual(got, []string{"low", "medium", "high"}) {
		t.Errorf("reasoning_efforts 应原样透传上游档位表，得到 %#v", entry["reasoning_efforts"])
	}
	if entry["default_reasoning_effort"] != "medium" {
		t.Errorf("default_reasoning_effort 应为 medium，得到 %#v", entry["default_reasoning_effort"])
	}
	if entry["can_disable_thinking"] != true {
		t.Errorf("can_disable_thinking 应为 true，得到 %#v", entry["can_disable_thinking"])
	}
	win, ok := entry["context_windows"].([]int64)
	if !ok || !reflect.DeepEqual(win, []int64{200000, 1000000}) {
		t.Errorf("context_windows 应原样透传档位表，得到 %#v", entry["context_windows"])
	}
	// 广告值与档位表并存：客户端既能显示默认窗口，也能列可选档。
	if entry["context_length"] != int64(1000000) {
		t.Errorf("context_length 应保留最大档，得到 %#v", entry["context_length"])
	}
}

// TestBuildModelEntryOmitsUndeclaredCapabilities 上游没声明就一个字段都不能冒出来。
// 这条是防造假的闸门：任何「我猜它支持 low/medium/high」的补全都会让本测试转红。
func TestBuildModelEntryOmitsUndeclaredCapabilities(t *testing.T) {
	mi := provider.ModelInfo{
		ID:            "qodercn/auto",
		ContextWindow: 200000,
		// 上游只声明 is_reasoning 一个 bool，没有档位表；也没有 context_config 多档。
		SupportsReasoning: true,
	}
	entry := (&Handler{}).buildModelEntry(provider.QoderCN, mi)

	for _, k := range []string{
		"reasoning_efforts",
		"default_reasoning_effort",
		"can_disable_thinking",
		"context_windows",
	} {
		if v, has := entry[k]; has {
			t.Errorf("上游未声明，不应输出 %q（得到 %#v）——这属于替上游编造能力", k, v)
		}
	}
	// 单一窗口仍应作为 context_length 声明（那是事实，不是编造）。
	if entry["context_length"] != int64(200000) {
		t.Errorf("context_length 应保留，得到 %#v", entry["context_length"])
	}
}

// TestBuildModelEntrySingleContextWindowIsNotAChoice 只有一档时没有「选择」语义，不输出档位表。
func TestBuildModelEntrySingleContextWindowIsNotAChoice(t *testing.T) {
	mi := provider.ModelInfo{ID: "m", ContextWindow: 128000, ContextWindows: []int64{128000}}
	entry := (&Handler{}).buildModelEntry(provider.QoderCOM, mi)
	if v, has := entry["context_windows"]; has {
		t.Errorf("单档不构成选择，不应输出 context_windows，得到 %#v", v)
	}
}

// TestBuildModelEntryCanDisableFalseIsDeclared false 是「上游明确说不可关闭」，
// 与 nil（未声明）语义不同，必须如实输出 —— 否则客户端会把不可关闭的模型当成可关。
func TestBuildModelEntryCanDisableFalseIsDeclared(t *testing.T) {
	no := false
	mi := provider.ModelInfo{ID: "m", SupportsReasoning: true, CanDisableThinking: &no}
	entry := (&Handler{}).buildModelEntry(provider.Minimax, mi)
	v, has := entry["can_disable_thinking"]
	if !has {
		t.Fatal("上游显式声明 false 时必须输出 can_disable_thinking")
	}
	if v != false {
		t.Errorf("can_disable_thinking 应为 false，得到 %#v", v)
	}
}
