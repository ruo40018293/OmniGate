package server

import (
	"encoding/json"
	"testing"
)

// TestUsageTokensAcceptsChannelBuiltInts 锁死 R64 的判据：usage 的数值必须同时认
// JSON 解出的 float64 **和**渠道在进程内自建的整型。
//
// 事故背景（2026-10-28 面板对账）：minimax 的 usage 由 anthropicStreamReader.finish
// 直接拼 map，值是 int64（promptTokens()/outTokens）；monkeycode 同理
// （s.inTokens/s.outTokens）。旧实现只做 `v.(float64)` 断言，于是这两个渠道
// 每一条成功请求都被记成 0 token —— 面板「消耗 token」偏低、流水 src=none、
// 「按模型消耗」里它们只有次数没有 token，且没有任何报错提示。
func TestUsageTokensAcceptsChannelBuiltInts(t *testing.T) {
	cases := []struct {
		name       string
		u          map[string]any
		wantPT     int64
		wantCT     int64
		wantSource string
	}{
		{"json-float64", map[string]any{"prompt_tokens": float64(120), "completion_tokens": float64(30)}, 120, 30, "upstream"},
		{"minimax-int64", map[string]any{"prompt_tokens": int64(120), "completion_tokens": int64(30)}, 120, 30, "upstream"},
		{"monkeycode-int64", map[string]any{"prompt_tokens": int64(7), "completion_tokens": int64(3)}, 7, 3, "upstream"},
		{"plain-int", map[string]any{"prompt_tokens": 120, "completion_tokens": 30}, 120, 30, "upstream"},
		{"int32", map[string]any{"prompt_tokens": int32(9), "completion_tokens": int32(1)}, 9, 1, "upstream"},
		{"json-number", map[string]any{"prompt_tokens": json.Number("2048"), "completion_tokens": json.Number("512")}, 2048, 512, "upstream"},
		{"anthropic-alias", map[string]any{"input_tokens": int64(11), "output_tokens": int64(22)}, 11, 22, "upstream"},
		{"mixed-types", map[string]any{"prompt_tokens": float64(5), "completion_tokens": int64(6)}, 5, 6, "upstream"},
		{"missing", nil, 0, 0, "none"},
		{"empty-object", map[string]any{}, 0, 0, "none"},
		{"zero-values", map[string]any{"prompt_tokens": int64(0), "completion_tokens": int64(0)}, 0, 0, "none"},
		{"garbage", map[string]any{"prompt_tokens": "120"}, 0, 0, "none"},
	}
	for _, c := range cases {
		pt, ct, src := usageTokens(c.u)
		if pt != c.wantPT || ct != c.wantCT || src != c.wantSource {
			t.Errorf("%s: got (pt=%d ct=%d src=%s), want (pt=%d ct=%d src=%s)",
				c.name, pt, ct, src, c.wantPT, c.wantCT, c.wantSource)
		}
	}
}

// TestUsageCreditKeepsFraction 锁死积分的两种形态：上游回的小数积分不能被截断，
// 进程内自建的整型积分也不能漏记。
func TestUsageCreditKeepsFraction(t *testing.T) {
	cases := []struct {
		name string
		u    map[string]any
		want float64
	}{
		{"float-fraction", map[string]any{"credit": 22.91}, 22.91},
		{"float-integer", map[string]any{"credit": float64(5)}, 5},
		{"int64", map[string]any{"credit": int64(7)}, 7},
		{"json-number", map[string]any{"credit": json.Number("1.25")}, 1.25},
		{"absent", map[string]any{}, 0},
		{"nil-map", nil, 0},
	}
	for _, c := range cases {
		if got := usageCredit(c.u); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
