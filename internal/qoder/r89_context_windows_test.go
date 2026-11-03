// r89_context_windows_test.go 锁住 R89：qoder 的 context_config 档位表
// 必须经 modelInfoOf 原样带到 provider.ModelInfo.ContextWindows。
package qoder

import (
	"reflect"
	"testing"
)

// TestModelInfoOfKeepsContextWindowTiers 多档：档位表保留，广告值取最大档。
func TestModelInfoOfKeepsContextWindowTiers(t *testing.T) {
	mi := modelInfoOf(DynamicModel{
		Key: "auto", DisplayName: "Auto",
		AvailableWindows: []int64{200000, 1000000},
	})
	if !reflect.DeepEqual(mi.ContextWindows, []int64{200000, 1000000}) {
		t.Errorf("ContextWindows 应保留上游档位表，得到 %#v", mi.ContextWindows)
	}
	if mi.ContextWindow != 1000000 {
		t.Errorf("ContextWindow 广告值应取最大档，得到 %d", mi.ContextWindow)
	}
	if !mi.ContextFromAPI {
		t.Error("档位表来自上游接口，ContextFromAPI 应为 true")
	}
}

// TestModelInfoOfSingleWindowIsNotATier 单档不写入 ContextWindows。
func TestModelInfoOfSingleWindowIsNotATier(t *testing.T) {
	mi := modelInfoOf(DynamicModel{Key: "solo", DisplayName: "Solo", AvailableWindows: []int64{200000}})
	if len(mi.ContextWindows) != 0 {
		t.Errorf("单档不应写入 ContextWindows，得到 %#v", mi.ContextWindows)
	}
	if mi.ContextWindow != 200000 {
		t.Errorf("ContextWindow 应为 200000，得到 %d", mi.ContextWindow)
	}
}

// TestModelInfoOfNoWindowTableStaysEmpty 上游无档位表时不编造，回落到 max_input_tokens。
func TestModelInfoOfNoWindowTableStaysEmpty(t *testing.T) {
	mi := modelInfoOf(DynamicModel{Key: "plain", DisplayName: "Plain", MaxInputTokens: 180000})
	if len(mi.ContextWindows) != 0 {
		t.Errorf("无档位表时不得编造 ContextWindows，得到 %#v", mi.ContextWindows)
	}
	if mi.ContextWindow != 180000 {
		t.Errorf("ContextWindow 应回落到 max_input_tokens，得到 %d", mi.ContextWindow)
	}
}

// TestModelInfoOfDoesNotAliasUpstreamSlice 档位表必须是拷贝，不能与上游切片共享底层数组。
func TestModelInfoOfDoesNotAliasUpstreamSlice(t *testing.T) {
	windows := []int64{200000, 1000000}
	mi := modelInfoOf(DynamicModel{Key: "auto", DisplayName: "Auto", AvailableWindows: windows})
	windows[0] = 1
	if mi.ContextWindows[0] != 200000 {
		t.Errorf("ContextWindows 与上游切片共享底层数组，已被改写为 %d", mi.ContextWindows[0])
	}
}
