// models.go 模型目录与倍率：解析 /v2/enterprises/personal/models，
// 抽出 cli agent 的模型列表（该接口同时返回 models[] 与 agents[]，
// cli agent 的 models 字段才是「可用于 CLI 的模型」权威顺序）。
package workbuddyai

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"omnigate/internal/auth"
	"omnigate/internal/provider"
)

// catalogModel 目录原始模型条目。
// 能力字段来自上游实测返回：supportsImages/supportsReasoning/supportsToolCall
// 与 disabledMultimodal（true 表示上游显式关闭多模态）。
type catalogModel struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Credits         string   `json:"credits"`
	MaxInputTokens  int64    `json:"maxInputTokens"`
	MaxOutputTokens int64    `json:"maxOutputTokens"`
	Disabled        bool     `json:"disabled"`
	Tags            []string `json:"tags"`

	SupportsImages     bool `json:"supportsImages"`
	SupportsReasoning  bool `json:"supportsReasoning"`
	SupportsToolCall   bool `json:"supportsToolCall"`
	DisabledMultimodal bool `json:"disabledMultimodal"`
}

// imageOK 判定该模型可否接收图像输入：
// 上游显式声明 supportsImages 且未被 disabledMultimodal 关闭。
func (m catalogModel) imageOK() bool {
	return m.SupportsImages && !m.DisabledMultimodal
}

// catalogResp 目录响应（只取所需字段）。
type catalogResp struct {
	Code int `json:"code"`
	Data struct {
		Models []catalogModel `json:"models"`
		Agents []struct {
			Name   string   `json:"name"`
			Models []string `json:"models"`
		} `json:"agents"`
		ModelPromotions []struct {
			Enabled  bool     `json:"enabled"`
			Model    []string `json:"modelIds"`
			Discount struct {
				Factor float64 `json:"factor"`
			} `json:"discount"`
			Badge struct {
				Label string `json:"label"`
				Color string `json:"color"`
			} `json:"badge"`
		} `json:"modelPromotions"`
	} `json:"data"`
}

// catalogCache 目录缓存：FetchModels 与 FetchModelPricing 共用，避免重复请求。
// v3 与 v2 分别缓存（两路独立探测、独立容错）。
type catalogCache struct {
	mu      sync.RWMutex
	models  []catalogModel
	promo   map[string]float64
	badges  map[string]string
	colors  map[string]string
	fetched time.Time

	// v2Down 记录 /v2/enterprises/personal/models 已不可用。2026-11-14 起上游
	// 对该端点返回 `404 {"error_msg":"404 Route Not Found"}`（本机实测，三个账号
	// 一致），此后直接走 v3，避免每轮刷新先白撞一次 404。到期后重探一次：上游
	// 若恢复 v2 就自动切回——v2 是 cli agent 的权威顺序，v3 只作兜底。
	v2Down    bool
	v2RetryAt time.Time

	v3        []catalogModel // /v3/config 全量模型面（不过滤）
	v3OK      bool           // v3 探测是否成功过（失败不写缓存，避免负缓存住坏结果）
	v3Fetched time.Time
}

const catalogTTL = 10 * time.Minute

// v2RetryInterval 判定 v2 下线后，多久再重探一次。
const v2RetryInterval = 30 * time.Minute

var cache catalogCache

// fetchCatalog 拉取目录（带 10 分钟缓存），返回 cli agent 过滤后的模型列表。
func (c *Client) fetchCatalog(a *auth.Auth) ([]catalogModel, error) {
	cache.mu.RLock()
	if len(cache.models) > 0 && time.Since(cache.fetched) < catalogTTL {
		out := cache.models
		cache.mu.RUnlock()
		return out, nil
	}
	cache.mu.RUnlock()

	env, fromV3, err := c.fetchCatalogEnv(a)
	if err != nil {
		return nil, err
	}

	// cli agent 的模型 ID 顺序为准；models[] 提供元数据。
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("models api: no cli agent models")
	}
	byID := make(map[string]catalogModel, len(env.Data.Models))
	for _, m := range env.Data.Models {
		byID[m.ID] = m
	}
	out := make([]catalogModel, 0, len(cliIDs))
	for _, id := range cliIDs {
		m, ok := byID[id]
		if !ok {
			m = catalogModel{ID: id, Name: id}
		}
		if m.Name == "" {
			m.Name = id
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api: empty cli list")
	}

	// 促销折扣：factor==0 表示当前免费；badge 供费率面板备注。
	promo := make(map[string]float64)
	badges := make(map[string]string)
	colors := make(map[string]string)
	for _, p := range env.Data.ModelPromotions {
		if !p.Enabled {
			continue
		}
		for _, id := range p.Model {
			promo[id] = p.Discount.Factor
			if p.Badge.Label != "" {
				badges[id] = p.Badge.Label
			}
			if p.Badge.Color != "" {
				colors[id] = p.Badge.Color
			}
		}
	}
	_ = badges

	cache.mu.Lock()
	cache.models, cache.promo, cache.badges, cache.colors, cache.fetched = out, promo, badges, colors, time.Now()
	// v3 兜底时把同一份响应灌进 v3 缓存：FetchModels / FetchModelPricing 随后调
	// fetchV3Catalog 做增强时直接命中缓存，不再对同一个端点重复请求一次。
	if fromV3 {
		cache.v3, cache.v3OK, cache.v3Fetched = env.Data.Models, true, time.Now()
	}
	cache.mu.Unlock()
	return out, nil
}

// fetchCatalogEnv 请求目录接口：v2 优先，404 后切 v3 并记住下线，避免每轮先撞 404。
// 其它错误（网络、401、5xx）原样返回、不做源切换——那类失败换端点救不回来，
// 悄悄换源只会把「账号过期」伪装成「目录为空」。
//
// 返回的 bool 表示这份响应是否来自 v3（供调用方回灌 v3 缓存）。
func (c *Client) fetchCatalogEnv(a *auth.Auth) (catalogResp, bool, error) {
	cache.mu.RLock()
	down, retryAt := cache.v2Down, cache.v2RetryAt
	cache.mu.RUnlock()

	if !down || time.Now().After(retryAt) {
		env, status, err := c.getCatalog(a, EpCatalog)
		switch {
		case err == nil:
			if down { // v2 恢复了：切回来，它才有 cli agent 的权威顺序
				cache.mu.Lock()
				cache.v2Down, cache.v2RetryAt = false, time.Time{}
				cache.mu.Unlock()
			}
			return env, false, nil
		case status == http.StatusNotFound:
			cache.mu.Lock()
			cache.v2Down, cache.v2RetryAt = true, time.Now().Add(v2RetryInterval)
			cache.mu.Unlock()
		default:
			return catalogResp{}, false, err
		}
	}

	env, _, err := c.getCatalog(a, EpCatalogV3)
	if err != nil {
		return catalogResp{}, false, err
	}
	return env, true, nil
}

// getCatalog 请求一个目录端点并解析外层信封。第二个返回值是 HTTP 状态码，
// 供调用方判 404（错误本身已类型化，状态码是额外的判据而非唯一信息源）。
func (c *Client) getCatalog(a *auth.Auth, path string) (catalogResp, int, error) {
	req, err := http.NewRequest(http.MethodGet, c.base()+path, nil)
	if err != nil {
		return catalogResp{}, 0, err
	}
	commonHeaders(req)
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return catalogResp{}, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		label := "models api"
		if path == EpCatalogV3 {
			label = "v3 config"
		}
		// 类型化错误：上层靠 errors.As 取分类才能 refresh + 重试（不变量 19），
		// 裸 fmt.Errorf 会让目录面的 401 自愈永远不生效（R79）。
		return catalogResp{}, resp.StatusCode, &provider.Error{
			Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode,
			Msg: fmt.Sprintf("%s: %s", label, truncate(string(raw), 200))}
	}
	var env catalogResp
	if err := json.Unmarshal(raw, &env); err != nil {
		return catalogResp{}, resp.StatusCode, fmt.Errorf("%s parse: %w", path, err)
	}
	if env.Code != 0 {
		return catalogResp{}, resp.StatusCode, fmt.Errorf("%s code=%d", path, env.Code)
	}
	return env, resp.StatusCode, nil
}

// fetchV3Catalog 抓取 /v3/config 全量模型面（2026-09-28 实测，issue #39）。
//
// 与 v2 的三点差异：
//   - 模型为**全量面**（22 个 vs cli 面 18 个），不经 agents[cli] 过滤——
//     extraModels 里的 deepseek-v4.1-flash/gpt-6-astra/kimi-k2.8-preview 只在这里有；
//   - **UA 门禁**：web UA → 400 code=12403 "check ua"，必须带 CLI 形 UA（clientUA 即可）；
//   - credits 可能为空串（如 default-model），合并时以「非空者优先」。
//
// 失败不影响 v2：两路独立容错，v3 挂了退化为原有行为（issue #39 的 unknown 依旧）。
func (c *Client) fetchV3Catalog(a *auth.Auth) ([]catalogModel, error) {
	cache.mu.RLock()
	if cache.v3OK && len(cache.v3) > 0 && time.Since(cache.v3Fetched) < catalogTTL {
		out := cache.v3
		cache.mu.RUnlock()
		return out, nil
	}
	cache.mu.RUnlock()

	// 请求/信封校验与主源共用 getCatalog（同一套 UA 门禁与类型化错误）。
	env, _, err := c.getCatalog(a, EpCatalogV3)
	if err != nil {
		return nil, err
	}
	if len(env.Data.Models) == 0 {
		return nil, fmt.Errorf("v3 config empty models")
	}

	cache.mu.Lock()
	cache.v3, cache.v3OK, cache.v3Fetched = env.Data.Models, true, time.Now()
	cache.mu.Unlock()
	return env.Data.Models, nil
}

// mergeV3 合并 v3 目录：v2 为主序与主数据源，v3 只补 v2 缺失的 id；
// 对两边都有的模型，v3 的 credits 非空时**覆盖** v2（v3 面更新，如 hy4-preview）。
//
// 返回 (合并后的模型表, v3 倍率表)。失败（v3 不可用）时返回 (v2 原样, nil)，
// 调用方无需感知 v3 是否参与——这正是「尽力增强，不拖累主路」的容错语义。
func mergeV3(v2 []catalogModel, v3 []catalogModel) ([]catalogModel, map[string]string) {
	if len(v3) == 0 {
		return v2, nil
	}
	byID := make(map[string]catalogModel, len(v3))
	for _, m := range v3 {
		byID[m.ID] = m
	}
	out := make([]catalogModel, 0, len(v2)+len(v3))
	seen := make(map[string]bool, len(v2)+len(v3))
	for _, m := range v2 {
		if vm, ok := byID[m.ID]; ok {
			if vm.Credits != "" {
				m.Credits = vm.Credits // v3 的倍率面更新
			}
			// 能力/上下文字段以 v3 为准（字段更全：maxAllowedSize/vendor/描述）
			if vm.MaxInputTokens > 0 {
				m.MaxInputTokens = vm.MaxInputTokens
			}
			if vm.MaxOutputTokens > 0 {
				m.MaxOutputTokens = vm.MaxOutputTokens
			}
			m.SupportsImages = vm.SupportsImages || m.SupportsImages
			m.SupportsToolCall = vm.SupportsToolCall || m.SupportsToolCall
		}
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	// v3 独有：追加在尾部（issue #39 的主角正是这批）
	for _, m := range v3 {
		if seen[m.ID] || m.ID == "" {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	rates := make(map[string]string, len(v3))
	for _, m := range v3 {
		if m.Credits != "" {
			rates[m.ID] = m.Credits
		}
	}
	return out, rates
}

// fetchPromotions 返回促销折扣表（factor==0 → 免费）。
func (c *Client) fetchPromotions(a *auth.Auth) map[string]float64 {
	if _, err := c.fetchCatalog(a); err != nil {
		return nil
	}
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	out := make(map[string]float64, len(cache.promo))
	for k, v := range cache.promo {
		out[k] = v
	}
	return out
}

// badgeOf 取模型促销标签（如 "Free now"）。
func badgeOf(id string) string {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	return cache.badges[id]
}

// colorOf 取模型促销标签的颜色（如 "#FF0000"），无则空。
func colorOf(id string) string {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	return cache.colors[id]
}

// ---------------------------------------------------------------------------
// 登录（与国内版路径相同，仅 host 不同）
// ---------------------------------------------------------------------------

// StartLogin 发起登录：POST auth/state 拿 state+授权 URL。
func (c *Client) StartLogin() (state, authURL string, err error) {
	req, err := http.NewRequest(http.MethodPost, c.base()+EpAuthState, strings.NewReader("{}"))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", originRef)
	req.Header.Set("Referer", originRef+"/")
	req.Header.Set("User-Agent", clientUA)
	req.Header.Set("X-No-Authorization", "true")
	req.Header.Set("X-No-User-Id", "true")
	req.Header.Set("X-No-Enterprise-Id", "true")
	req.Header.Set("X-No-Department-Info", "true")
	data, err := c.doJSON(req)
	if err != nil {
		return "", "", fmt.Errorf("auth state failed: %w", err)
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		return "", "", fmt.Errorf("auth state: missing state or authUrl")
	}
	return st.State, st.AuthURL, nil
}

// PollToken 轮询登录 token。未完成时返回 nil, nil（对应上游 code=11217）。
// 成功时返回凭据字段。
type LoginResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
	Domain       string
	UID          string
	EnterpriseID string
	Nickname     string
}

// PollLogin 单次轮询；pending 为 true 表示浏览器尚未完成。
func (c *Client) PollLogin(state string) (r LoginResult, pending bool, err error) {
	req, err := http.NewRequest(http.MethodGet, c.base()+EpAuthToken+state, nil)
	if err != nil {
		return r, false, err
	}
	commonHeaders(req)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("X-No-Authorization", "true")
	req.Header.Set("X-No-User-Id", "true")
	req.Header.Set("X-No-Enterprise-Id", "true")
	req.Header.Set("X-No-Department-Info", "true")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return r, false, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env apiEnvelope
	if json.Unmarshal(raw, &env) != nil {
		return r, false, fmt.Errorf("token parse failed: %s", truncate(string(raw), 120))
	}
	if env.Code == 11217 {
		return r, true, nil // login ing...
	}
	if env.Code != 0 {
		return r, false, fmt.Errorf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(env.Data, &tok); err != nil || tok.AccessToken == "" {
		return r, true, nil // 尚未返回可用 token，视为 pending
	}
	r.AccessToken, r.RefreshToken, r.ExpiresIn = tok.AccessToken, tok.RefreshToken, tok.ExpiresIn
	r.Domain = tok.Domain
	// 取账号信息（带 Bearer）；失败不影响登录结果。
	acct := c.fetchAccount(state, tok.AccessToken)
	r.UID, r.EnterpriseID, r.Nickname = acct[0], acct[1], acct[2]
	return r, false, nil
}

// fetchAccount GET login/account 取 uid/enterpriseId/nickname。
func (c *Client) fetchAccount(state, accessToken string) [3]string {
	req, err := http.NewRequest(http.MethodGet, c.base()+EpLoginAcct+state, nil)
	if err != nil {
		return [3]string{}
	}
	commonHeaders(req)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	data, err := c.doJSON(req)
	if err != nil {
		return [3]string{}
	}
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	_ = json.Unmarshal(data, &acct)
	return [3]string{acct.UID, acct.EnterpriseID, acct.Nickname}
}

// ---------------------------------------------------------------------------
// 错误分类
// ---------------------------------------------------------------------------

// hardMarkers 余额/权益不足关键词（小写比较 + 中文原文双通道）。
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	// R93：日额度耗尽不是「请求错误」。2026-11-04 实测 qodercn 返回 403
	// `Billing daily count exceeded`，此前落到 ErrClient（可轮转）——每个渠道只有
	// 1 个号，换号必败，最后变成一个毫无信息量的 503。归到 ErrHardCredit 才对：
	// 冷却到日重置，且不会为了换个号白跑一趟。
	"not enough credit", "billing daily count exceeded", "daily count exceeded", "次数已达上限",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

// sessionDeadMarkers 会话失效标记（需重新登录）。
var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// Classify 按 HTTP 状态码 + body 判定错误类别（与国内版分类语义一致）。
func Classify(status int, body string) provider.ErrKind {
	if status == http.StatusPaymentRequired {
		return provider.ErrHardCredit
	}
	lower := strings.ToLower(body)
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) {
			return provider.ErrSessionDead
		}
	}
	// 429 优先于 hardRule：限流 body 高频带 "quota exceeded"。
	if status == http.StatusTooManyRequests {
		// 「模型繁忙」（429 + code=14003 / "Model busy"）：共享免费池瞬时饱和，
		// 与请求头、账号健康都无关（实测见 retry.go:modelBusy）。
		// 归为请求级透传：客户端拿到 429 自行退避即可，**不得**冷却模型 60s——
		// 那会把瞬时抖动放大成整分钟的 no_healthy_account（issue #53 同款坑）。
		if modelBusy(status, []byte(body)) {
			return provider.ErrPassthrough
		}
		// 上游按**模型**限流（6004 / "switch to the other models"）：属请求级拒绝，
		// **不得**按账号级冷却——否则该账号上其它还能用的模型一起被拖下水
		// （issue #53：A 的 deepseek-v4.1-flash 撞每日上限后，A 的 gpt-5.6-luna 也不能用）。
		// 透传原文让客户端按 Retry-After 自行退避或换模型，与 R16 单账号渠道同款哲学。
		if strings.Contains(lower, "switch to the other models") || strings.Contains(lower, "usage exceeds frequency limit") {
			return provider.ErrPassthrough
		}
		return provider.ErrSoftRate
	}
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return provider.ErrHardCredit
		}
	}
	// 非 429 但 body 含限流文案 → 软限流
	if strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "usage limit") || strings.Contains(lower, "请求过于频繁") {
		// 同上：模型级限流不算账号问题。
		if strings.Contains(lower, "switch to the other models") {
			return provider.ErrPassthrough
		}
		return provider.ErrSoftRate
	}
	if status == http.StatusNotFound {
		return provider.ErrNotFound
	}
	if status >= 500 {
		return provider.ErrServer
	}
	if status >= 400 {
		if strings.Contains(lower, "blocked by security policy") ||
			strings.Contains(lower, "unapproved channel") ||
			strings.Contains(lower, "illegal api invocation") {
			return provider.ErrContentBlocked
		}
		if (status == 400 || status == 404) &&
			(strings.Contains(lower, "prompt is too long") || strings.Contains(lower, "11115")) {
			return provider.ErrPromptTooLong
		}
		if status == 403 && strings.TrimSpace(body) == "" {
			return provider.ErrWafBlock
		}
		if strings.Contains(lower, "request illegal") || strings.Contains(lower, "trial not activated") {
			return provider.ErrAccountFault
		}
		return provider.ErrClient
	}
	return provider.ErrNone
}
