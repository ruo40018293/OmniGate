// Package login_cline 把用户在面板粘贴的 Cline 官方 API Key 落成本工具凭据。
//
// Cline 没有可编程登录：凭据是官方文档化的 `sk_` API Key
// （app.cline.bot → Account → API Keys 创建），粘贴即用、无 refresh 流程。
// 因此这里不做 OAuth，只做「校验 + 原子落盘」两件事：
//
//	POST /api/login/cline_key { "key": "sk_..." }  →  app.SubmitClineKey
//
// 校验走 cline.ValidateKey：只调 GET /api/v1/api-keys（零 token 消耗、零副作用），
// 能拿到稳定 uid（apk- 前缀）与同账号兄弟 Key 前缀（用于「同账号多 Key 不扩额度」告警）。
package login_cline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"omnigate/internal/auth"
	"omnigate/internal/cline"
)

// SaveAuth 校验 key 并写入 auths/cline-<uid>.json。
//
// 返回 uid 与探测结果（调用方用来提示「同账号多 Key 不扩额度」）。
// expiresAt 取远期常量：Cline 的 Key 不轮换也不过期，
// 留 0 会让 auth.NeedsRefresh 恒真 → 每个请求都去「刷新」一个不存在的 refresh 流程。
func SaveAuth(authDir, key string) (string, *cline.ProbeResult, error) {
	return saveAuth(authDir, key, "")
}

// saveAuth 是 SaveAuth 的可注入实现（base 非空时改用假上游，供单测）。
func saveAuth(authDir, key, base string) (string, *cline.ProbeResult, error) {
	key = strings.TrimSpace(key)
	var (
		pr  *cline.ProbeResult
		err error
	)
	if strings.TrimSpace(base) == "" {
		pr, err = cline.ValidateKey(key)
	} else {
		pr, err = cline.ValidateKeyWithBase(base, key)
	}
	if err != nil {
		return "", nil, err
	}
	if pr == nil || strings.TrimSpace(pr.UID) == "" {
		return "", nil, fmt.Errorf("上游未返回账号标识，无法保存（请确认 Key 有效）")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken": auth.EncryptSecret(key),
			"expiresAt":   cline.FarFutureExpiry(),
			"domain":      cline.APIBase,
		},
		"account": map[string]any{
			"uid":      pr.UID,
			"nickname": pr.AccountName,
		},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		return "", nil, err
	}
	fp := filepath.Join(authDir, "cline-"+pr.UID+".json")
	tmp := fp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", nil, err
	}
	if err := os.Rename(tmp, fp); err != nil {
		return "", nil, err
	}
	return pr.UID, pr, nil
}
