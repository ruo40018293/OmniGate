// r89_context_windows_test.go 锁住 R89：qodercn 的 context_config 档位表
// 必须原样带到 provider.ModelInfo.ContextWindows。
package qodercn

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

// TestToModelInfosSingleWindowIsNotATier 单档不写入 ContextWindows。
func TestToModelInfosSingleWindowIsNotATier(t *testing.T) {
	got := toModelInfos([]ModelEntry{{
		Key: "solo", DisplayName: "Solo",
		AvailableWindows: []int64{200000},
	}})
	if len(got[0].ContextWindows) != 0 {
		t.Errorf("单档不应写入 ContextWindows，得到 %#v", got[0].ContextWindows)
	}
}

// TestToModelInfosNoWindowTableStaysEmpty 上游没有档位表时不编造。
func TestToModelInfosNoWindowTableStaysEmpty(t *testing.T) {
	got := toModelInfos([]ModelEntry{{Key: "plain", DisplayName: "Plain", MaxInputTokens: 180000}})
	if len(got[0].ContextWindows) != 0 {
		t.Errorf("无档位表时不得编造 ContextWindows，得到 %#v", got[0].ContextWindows)
	}
}
