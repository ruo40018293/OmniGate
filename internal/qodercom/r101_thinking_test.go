// r101_thinking_test.go 锁住 R101：qoder 系上游 `thinking_config` 的档位表
// 必须解析进模型声明，客户端才长得出思考档位下拉框。
//
// 探针实证（2026-11-11，真实凭据直连 /algo/api/v2/model/list?Encode=1，响应 60151 字节）：
//
//	auto      无 thinking_config                                   ⇒ efforts=[]、default=""
//	ultimate  efforts{low,medium,high,xhigh,max} 默认 high
//	efficient 无 thinking_config                                   ⇒ efforts=[]、default=""
//	gfmodel   efforts{high,max} 默认 max
//	mmodel    efforts{low,medium,high,xhigh,max} 默认 xhigh
//
// 上游 efforts 是 **map**，JSON 解析顺序随机；不排序的话同一个模型每次启动给出的
// 下拉框顺序都不同，故必须按 low→medium→high→xhigh→max 固定。
package qodercom

import (
	"encoding/json"
	"reflect"
	"testing"
)

// thWire 从原始 JSON 造一条 modelWire。
func thWire(t *testing.T, raw string) modelWire {
	t.Helper()
	var w modelWire
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		t.Fatalf("夹具解析失败: %v", err)
	}
	return w
}

// TestParseThinkingConfigSortsBySemanticRank 档位按「低→高」语义排序，不按 map 随机序。
func TestParseThinkingConfigSortsBySemanticRank(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		want      []string
		wantDeflt string
	}{
		{
			name:      "ultimate 五档默认 high",
			raw:       `{"thinking_config":{"enabled":{"efforts":{"high":{"is_default":true},"low":{},"max":{},"medium":{},"xhigh":{}},"is_default":true}}}`,
			want:      []string{"low", "medium", "high", "xhigh", "max"},
			wantDeflt: "high",
		},
		{
			name:      "gfmodel 两档默认 max",
			raw:       `{"thinking_config":{"enabled":{"efforts":{"high":{},"max":{"is_default":true}},"is_default":true}}}`,
			want:      []string{"high", "max"},
			wantDeflt: "max",
		},
		{
			name:      "qmodel 三档默认 xhigh（xhigh 排 high 之后）",
			raw:       `{"thinking_config":{"enabled":{"efforts":{"xhigh":{"is_default":true},"low":{},"high":{}},"is_default":true}}}`,
			want:      []string{"low", "high", "xhigh"},
			wantDeflt: "xhigh",
		},
		{
			name:      "ultra 是已知最高档，排 max 之后",
			raw:       `{"thinking_config":{"enabled":{"efforts":{"ultra":{},"max":{},"minimal":{}},"is_default":true}}}`,
			want:      []string{"minimal", "max", "ultra"},
			wantDeflt: "ultra", // 未标 is_default ⇒ 回退最高档
		},
		{
			name:      "无 thinking_config",
			raw:       `{"key":"auto"}`,
			want:      nil,
			wantDeflt: "",
		},
		{
			name:      "thinking_config 为 null",
			raw:       `{"thinking_config":null}`,
			want:      nil,
			wantDeflt: "",
		},
		{
			name:      "只有 disabled 没有 enabled",
			raw:       `{"thinking_config":{"disabled":{"reason":"x"}}}`,
			want:      nil,
			wantDeflt: "",
		},
		{
			name:      "efforts 空对象",
			raw:       `{"thinking_config":{"enabled":{"efforts":{},"is_default":true}}}`,
			want:      nil,
			wantDeflt: "",
		},
		{
			name:      "空档位名被丢弃",
			raw:       `{"thinking_config":{"enabled":{"efforts":{"":{"is_default":true},"low":{},"high":{}},"is_default":true}}}`,
			want:      []string{"low", "high"},
			wantDeflt: "high",
		},
		{
			name:      "未知档位排在已知之后（字典序）",
			raw:       `{"thinking_config":{"enabled":{"efforts":{"zzz":{},"high":{},"aaa":{},"low":{}},"is_default":true}}}`,
			want:      []string{"low", "high", "aaa", "zzz"},
			wantDeflt: "zzz",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := thWire(t, tc.raw)
			got, gotDeflt := w.parseThinkingConfig()
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("efforts = %#v，期望 %#v（顺序必须低→高，否则下拉框每次启动都在跳）", got, tc.want)
			}
			if gotDeflt != tc.wantDeflt {
				t.Errorf("default = %q，期望 %q", gotDeflt, tc.wantDeflt)
			}
		})
	}
}

// TestParseDynamicModelsCarriesThinkingIntoModelInfo 解析链一路带进 provider.ModelInfo，
// 否则 /v1/models 出口（buildModelEntry）拿不到数据，客户端下拉框仍是空的。
func TestParseDynamicModelsCarriesThinkingIntoModelInfo(t *testing.T) {
	raw := map[string]json.RawMessage{
		"chat": json.RawMessage(`[
			{"key":"ultimate","display_name":"Ultimate","enable":true,"is_reasoning":true,
			 "context_config":{"1M":{"token_count":1000000}},
			 "thinking_config":{"enabled":{"efforts":{"high":{"is_default":true},"low":{},"max":{}},"is_default":true}}},
			{"key":"auto","display_name":"Auto","enable":true,
			 "context_config":{"200K":{"is_default":true,"token_count":200000}}}
		]`),
	}
	entries, err := parseDynamicModels(raw)
	if err != nil {
		t.Fatalf("parseDynamicModels: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("应解析出 2 条，得到 %d", len(entries))
	}
	byKey := map[string]ModelEntry{}
	for _, e := range entries {
		byKey[e.Key] = e
	}
	ult := byKey["ultimate"]
	if !reflect.DeepEqual(ult.ReasoningEfforts, []string{"low", "high", "max"}) {
		t.Errorf("ultimate 档位 = %#v，期望 [low high max]", ult.ReasoningEfforts)
	}
	if ult.DefaultReasoningEffort != "high" {
		t.Errorf("ultimate 默认档 = %q，期望 high", ult.DefaultReasoningEffort)
	}
	auto := byKey["auto"]
	if len(auto.ReasoningEfforts) != 0 {
		t.Errorf("auto 上游未声明档位，不得编造，得到 %#v", auto.ReasoningEfforts)
	}
	if auto.DefaultReasoningEffort != "" {
		t.Errorf("auto 默认档应为空，得到 %q", auto.DefaultReasoningEffort)
	}

	// /v1/models 出口
	for _, mi := range toModelInfos(entries) {
		switch mi.ID {
		case "ultimate":
			if !reflect.DeepEqual(mi.ReasoningEfforts, []string{"low", "high", "max"}) {
				t.Errorf("ModelInfo 档位丢失: %#v", mi.ReasoningEfforts)
			}
			if mi.DefaultReasoningEffort != "high" {
				t.Errorf("ModelInfo 默认档丢失: %q", mi.DefaultReasoningEffort)
			}
		case "auto":
			if len(mi.ReasoningEfforts) != 0 || mi.DefaultReasoningEffort != "" {
				t.Errorf("auto 不得声明档位: %#v / %q", mi.ReasoningEfforts, mi.DefaultReasoningEffort)
			}
		}
	}
}

// TestSortEffortsStableForDuplicates 重复档位不能互相换位（SliceStable）+ 已知档位齐全。
func TestSortEffortsStableForDuplicates(t *testing.T) {
	got := []string{"max", "low", "max", "medium", "low"}
	sortEfforts(got)
	want := []string{"low", "low", "medium", "max", "max"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sortEfforts = %#v，期望 %#v", got, want)
	}
}

// TestEffortRankCoversAllUpstreamLevels 上游出现过的档位都必须有明确次序，
// 否则会掉进「未知档位按字典序排」的分支，把 max 排到 high 前面。
func TestEffortRankCoversAllUpstreamLevels(t *testing.T) {
	for _, lvl := range []string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"} {
		if _, ok := effortRank[lvl]; !ok {
			t.Errorf("档位 %q 未登记 rank（会被当未知档按字典序排）", lvl)
		}
	}
	if !(effortRank["minimal"] < effortRank["low"] &&
		effortRank["low"] < effortRank["medium"] &&
		effortRank["medium"] < effortRank["high"] &&
		effortRank["high"] < effortRank["xhigh"] &&
		effortRank["xhigh"] < effortRank["max"] &&
		effortRank["max"] < effortRank["ultra"]) {
		t.Error("rank 必须严格递增 minimal<low<medium<high<xhigh<max<ultra")
	}
}
