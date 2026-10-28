package app

import (
	"testing"

	"omnigate/internal/provider"
)

// TestBuildFeesChannelsKeepsUnpricedNewChannels 守住 R65 的第二半：
// 即使渠道已登记进 feeChannelOrder，只要上游不给倍率，后端也必须把模型行交出去
// （Priced=false），由前端决定怎么显示。后端如果因为「没定价就跳过」，面板会整组空白，
// 前端再修也救不回来。
func TestBuildFeesChannelsKeepsUnpricedNewChannels(t *testing.T) {
	modelsByKind := map[provider.Kind][]provider.ModelInfo{
		provider.Minimax: {{ID: "MiniMax-M3"}, {ID: "MiniMax-M3.1-Flash"}},
		provider.Cline:   {{ID: "cline-free/solar-mini4"}},
	}
	// 不给任何定价（模拟 minimax/cline 上游无费率端点）
	channels := buildFeesChannels(modelsByKind, nil, feeChannelOrder)

	got := map[string][]feesModelRow{}
	for _, ch := range channels {
		got[ch.Channel] = ch.Models
	}
	for _, want := range []struct {
		ch    string
		count int
	}{
		{"minimax", 2},
		{"cline", 1},
	} {
		rows, ok := got[want.ch]
		if !ok {
			t.Fatalf("渠道 %s 未出现在费率数据里（R65：面板会整组空白）", want.ch)
		}
		if len(rows) != want.count {
			t.Fatalf("渠道 %s 模型行数=%d，want %d", want.ch, len(rows), want.count)
		}
		for _, r := range rows {
			if r.Priced {
				t.Fatalf("渠道 %s 模型 %s 被判为已定价，但上游没有倍率数据（不能拿 0 冒充）", want.ch, r.Model)
			}
		}
	}
}
