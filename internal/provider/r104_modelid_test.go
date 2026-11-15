package provider

// R104 PublicModelID / ModelNamespaces 的契约闸门。
//
// 背景：`PublicModelID` 是三处输出的单一真源——面板「复制模型名」、`/v1/models`
// 的 id 字段、报错提示里的可用模型名。三处不一致会产生「用户复制了却拿不到」的
// 怪现象，而且不报错。
//
// 两种形态必须同时成立：
//  1. 上游 ID 自带命名空间（首段命中 ModelNamespaces 且映射到同一渠道）⇒ **整串透传**。
//     cline-free/* 就是这种：上游的 ID 本身就是 `cline-free/mimo-v2.6-flash`，
//     再拼一层 `cline/` 会得到 `cline/cline-free/mimo-v2.6-flash`——老配置要兼容，
//     但**新输出**必须是透传形态（否则上游收到的模型名里多了前缀）。
//  2. 其余渠道拼 `<kind>/<上游ID>`，且上游 ID 里本来就带 `/`（如 `nvidia/xxx:free`）
//     也不能被当成命名空间——命名空间只认 ModelNamespaces 里声明的那些。

import "testing"

func TestPublicModelID(t *testing.T) {
	tests := []struct {
		name string
		kind Kind
		up   string
		want string
	}{
		{"普通渠道拼前缀", WorkBuddy, "glm-5.2", "workbuddy/glm-5.2"},
		{"上游 ID 自带斜杠但不是命名空间", Kilo, "nvidia/nemotron-3.5-super-vl:free", "kilo/nvidia/nemotron-3.5-super-vl:free"},
		{"命名空间直通", Cline, "cline-free/mimo-v2.6-flash", "cline-free/mimo-v2.6-flash"},
		{"命名空间不属于本渠道则不直通", WorkBuddy, "cline-free/mimo", "workbuddy/cline-free/mimo"},
		{"上游 ID 有空白", ZCode, "  glm-5  ", "zcode/glm-5"},
		{"首段为空的斜杠", Kilo, "/leading", "kilo//leading"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PublicModelID(tc.kind, tc.up); got != tc.want {
				t.Errorf("PublicModelID(%s, %q) = %q，预期 %q", tc.kind, tc.up, got, tc.want)
			}
		})
	}
}

// TestModelNamespacesMapToRealKinds 命名空间表里的每个 kind 都必须是已声明的渠道。
// 写成不存在的 kind 时 PublicModelID 永远不直通，而且是静默的。
func TestModelNamespacesMapToRealKinds(t *testing.T) {
	known := map[Kind]bool{
		WorkBuddy: true, WorkBuddyAI: true, TraeWork: true, TraeCode: true,
		Qoder: true, QoderCN: true, QoderCOM: true, QwenWork: true,
		MonkeyCode: true, Raccoon: true, Loomy: true, GLM: true,
		Minimax: true, Cline: true, Kilo: true,
		ZCode: true,
	}
	for ns, k := range ModelNamespaces {
		if !known[k] {
			t.Errorf("ModelNamespaces[%q] = %q，不是已声明的渠道", ns, k)
		}
		if ns == "" {
			t.Error("ModelNamespaces 里不能有空命名空间（会让所有以 / 开头的 ID 误判）")
		}
	}
}
