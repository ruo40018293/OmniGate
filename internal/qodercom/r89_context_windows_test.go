// r89_context_windows_test.go 锁住 R89：上游 context_config 的档位表必须原样
// 带到 provider.ModelInfo.ContextWindows，/v1/models 才有东西可声明。
//
// 之前只在 ContextWindow 里塞了「最大档」（广告值），档位表本身被丢掉，
// 于是 qoder 系明明支持按档切换（客户端传 context_length 选档），
// 用户在自己的客户端里却看不到任何可选值。
package qodercom

import (
	"reflect"
	"testing"
)

// TestToModelInfosKeepsContextWindowTiers 多档：ContextWindows 原样保留，广告值取最大档。
func TestToModelInfosKeepsContextWindowTiers(t *testing.T) {
	got := toModelInfos([]ModelEntry{{
		Key: "auto", DisplayName: "Auto",
		AvailableWindows: []int64{200000, 1000000},
	}})
	if len(got) != 1 {
		t.Fatalf("应产出 1 条，得到 %d", len(got))
	}
	if !reflect.DeepEqual(got[0].ContextWindows, []int64{200000, 1000000}) {
		t.Errorf("ContextWindows 应保留上游档位表，得到 %#v", got[0].ContextWindows)
	}
	if got[0].ContextWindow != 1000000 {
		t.Errorf("ContextWindow 广告值应取最大档，得到 %d", got[0].ContextWindow)
	}
}

// TestToModelInfosSingleWindowIsNotATier 单档不写入 ContextWindows（没有选择意义）。
func TestToModelInfosSingleWindowIsNotATier(t *testing.T) {
	got := toModelInfos([]ModelEntry{{
		Key: "solo", DisplayName: "Solo",
		AvailableWindows: []int64{200000},
	}})
	if len(got[0].ContextWindows) != 0 {
		t.Errorf("单档不应写入 ContextWindows，得到 %#v", got[0].ContextWindows)
	}
	if got[0].ContextWindow != 200000 {
		t.Errorf("ContextWindow 应为 200000，得到 %d", got[0].ContextWindow)
	}
}

// TestToModelInfosNoWindowTableStaysEmpty 上游没有 context_config 时不编造档位。
func TestToModelInfosNoWindowTableStaysEmpty(t *testing.T) {
	got := toModelInfos([]ModelEntry{{Key: "plain", DisplayName: "Plain", MaxInputTokens: 180000}})
	if len(got[0].ContextWindows) != 0 {
		t.Errorf("无档位表时不得编造 ContextWindows，得到 %#v", got[0].ContextWindows)
	}
}

// TestToModelInfosDoesNotAliasUpstreamSlice 档位表必须是拷贝——
// 上游数据后续被复用/改写时不能反向污染已产出并缓存的 ModelInfo。
func TestToModelInfosDoesNotAliasUpstreamSlice(t *testing.T) {
	windows := []int64{200000, 1000000}
	got := toModelInfos([]ModelEntry{{Key: "auto", DisplayName: "Auto", AvailableWindows: windows}})
	windows[0] = 1
	if got[0].ContextWindows[0] != 200000 {
		t.Errorf("ContextWindows 与上游切片共享底层数组，已被改写为 %d", got[0].ContextWindows[0])
	}
}
