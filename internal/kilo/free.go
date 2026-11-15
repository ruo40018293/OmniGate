package kilo

import (
	"strings"

	"omnigate/internal/provider"
)

// 上游清单里的默认容量。这两个数只用于「上游没给」时的兜底展示，且必须
// 通过 ContextFromAPI=false 明确标成估值 —— 冒充官方值是 R89 明确禁止的做法。
const (
	defaultContextWindow int64 = 131072
	defaultMaxTokens     int64 = 32768
)

// listingRow 是 GET /models 的 data[] 元素（只取本渠道用得到的字段）。
//
// 免费判据只看 IsFree：上游对同一模型族既有付费也有免费条目，按名字听不出区别
// （例如 nvidia/nemotron-3.5-super-vl 有付费也有 :free），只有这个布尔是权威的。
type listingRow struct {
	ID                  string          `json:"id"`
	Name                string          `json:"name"`
	IsFree              bool            `json:"isFree"`
	ContextLength       int64           `json:"context_length"`
	SupportedParameters []string        `json:"supported_parameters"`
	TopProvider         topProviderInfo `json:"top_provider"`
	MaxOutput           int64           `json:"max_output"`
	Architecture        architecture    `json:"architecture"`
}

// architecture 是清单的 architecture 块。
//
// InputModalities 是**视觉能力的唯一可靠来源**：实测 14 个免费模型里有 6 个
// 能读图（`nemotron-3.5-super-vl`、`ling-3.2-flash`、`particle-1` 等），
// 但 supported_parameters 里一个 vision/image 都没有 —— 只看 supported_parameters
// 会把这 6 个全标成纯文本，用户发图必失败（R88 同型：能读图的被声明成 text-only）。
type architecture struct {
	InputModalities []string `json:"input_modalities"`
}

// topProviderInfo 是清单的 top_provider 块。
//
// MaxCompletionTokens 是**输出上限的权威来源**：实测 14 个免费模型的顶层
// `max_output` 字段全部缺失（null），输出上限只在 top_provider 里给。
// 只读顶层会得到 maxTokens = 本地默认值，且 ContextFromAPI 永远是 false
// —— 面板上所有模型都显示成「估值」，等于把上游声明扔了。
type topProviderInfo struct {
	ContextLength       int64 `json:"context_length"`
	MaxCompletionTokens int64 `json:"max_completion_tokens"`
}

// IsFreeRow 判一条清单记录是否属于免费池：既要是免费，也要有可路由的 id。
func IsFreeRow(r listingRow) bool {
	return r.IsFree && strings.TrimSpace(r.ID) != ""
}

// rowToModelInfo 把清单记录转成统一模型描述。
//
// 能力位只从 supported_parameters 认，不从模型名猜：上游这份列表里名字相同、
// 供应商相同但能力不同的模型很常见（`*-reasoning` 只是命名习惯，不是契约）。
func rowToModelInfo(r listingRow) provider.ModelInfo {
	ctx, fromAPI := r.ContextLength, r.ContextLength > 0
	if ctx <= 0 && r.TopProvider.ContextLength > 0 {
		// 顶层没给时退到 top_provider 的声明，这同样是上游数据。
		ctx, fromAPI = r.TopProvider.ContextLength, true
	}
	// 输出上限：先看顶层 max_output，再退 top_provider.max_completion_tokens
	// （实测后者才是上游真正下发的字段）。
	maxOut := r.MaxOutput
	if maxOut <= 0 {
		maxOut = r.TopProvider.MaxCompletionTokens
	}
	fromMaxOut := maxOut > 0
	if ctx <= 0 {
		ctx = defaultContextWindow
	}
	if maxOut <= 0 {
		maxOut = defaultMaxTokens
	}
	mi := provider.ModelInfo{
		ID:            r.ID,
		Name:          displayName(r),
		ContextWindow: ctx,
		MaxTokens:     maxOut,
		// 两个标志必须**同时**成立才算官方值：上游常见「给了 max_output 却没给
		// context_length」，这种情况上下文仍是我们填的默认值，不能标成上游声明。
		ContextFromAPI:    fromAPI && fromMaxOut,
		SupportsReasoning: hasParam(r.SupportedParameters, "reasoning") || hasParam(r.SupportedParameters, "reasoning_effort"),
		SupportsTools:     hasParam(r.SupportedParameters, "tools") && hasParam(r.SupportedParameters, "tool_choice"),
		// 视觉能力看两处：architecture.input_modalities 含 image（实测唯一可靠来源），
		// 或 supported_parameters 显式声明 vision/image/image_url（部分模型两处都有）。
		SupportsImages: hasModality(r.Architecture.InputModalities, "image") ||
			hasParam(r.SupportedParameters, "vision") ||
			hasParam(r.SupportedParameters, "image") ||
			hasParam(r.SupportedParameters, "image_url"),
	}
	return mi
}

func displayName(r listingRow) string {
	if n := strings.TrimSpace(r.Name); n != "" {
		return n
	}
	return r.ID
}

// hasModality 判 architecture.input_modalities 是否含某个模态。
func hasModality(mods []string, want string) bool {
	return hasParam(mods, want)
}

func hasParam(params []string, want string) bool {
	for _, p := range params {
		if strings.EqualFold(strings.TrimSpace(p), want) {
			return true
		}
	}
	return false
}

// staticFree 是清单拉不到时的兜底，**只收录有公开出处的条目**。
//
// 只放 `kilo-auto/free` 一条：它是参考实现的 README 里唯一写明的免费自动路由模型，
// 其余「免费」模型随上游促销变化，写死在代码里就会变成过期广告——用户点开发现
// 用不了，比列表短更糟。ContextFromAPI 明确为 false：这两个数值是估值不是声明。
func staticFree() []provider.ModelInfo {
	return []provider.ModelInfo{{
		ID:             "kilo-auto/free",
		Name:           "Kilo Auto (free)",
		ContextWindow:  defaultContextWindow,
		MaxTokens:      defaultMaxTokens,
		ContextFromAPI: false,
	}}
}

// StaticModels 供 main 装配 Runtime.StaticModels 用（清单不可用时的兜底）。
func StaticModels() []provider.ModelInfo { return staticFree() }

// filterFree 从清单里挑出免费模型，并叠加静态兜底的条目。
//
// 排序与去重规则：
//   - 保持上游顺序，不重排（上游的默认推荐序就体现在这里，重排等于替它做主）；
//   - 按 id 去重，先出现的优先；
//   - 静态条目若上游也有，以**上游声明为基底**并只补静态里有、上游没声明的能力位 ——
//     反过来让静态覆盖上游会把上游新增的工具/视觉能力抹掉（面板会显示「不支持」）。
func filterFree(rows []listingRow) []provider.ModelInfo {
	out := make([]provider.ModelInfo, 0, len(rows)+1)
	seen := make(map[string]int, len(rows))
	for _, r := range rows {
		if !IsFreeRow(r) {
			continue
		}
		id := strings.TrimSpace(r.ID)
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = len(out)
		out = append(out, rowToModelInfo(r))
	}
	// 静态兜底里上游没给的那些补在末尾（有就跳过，绝不覆盖上游声明）。
	for _, s := range staticFree() {
		if idx, ok := seen[s.ID]; ok {
			mi := out[idx]
			mi.SupportsImages = mi.SupportsImages || s.SupportsImages
			mi.SupportsReasoning = mi.SupportsReasoning || s.SupportsReasoning
			mi.SupportsTools = mi.SupportsTools || s.SupportsTools
			out[idx] = mi
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return staticFree()
	}
	return out
}
