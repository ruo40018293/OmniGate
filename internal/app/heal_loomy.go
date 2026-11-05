package app

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// loomyExpirySentinel 返回 Loomy 凭据的远期到期哨兵（约 50 年后）。
//
// 为什么用哨兵而不是「真实过期时刻」：Loomy 上游**没有 refresh 端点**，
// auth.NeedsRefreshLocked 只看 ExpiresAt、不看 refreshToken，所以任何短期时刻
// 一到就会让 NeedsRefresh 恒真 → loomy.RefreshToken 恒返回 ErrSessionDead
// → 面板红字「token refresh failed」+ 该号被跳过 + 每 10 分钟一次必然失败的刷新。
//
// 上游真实行为（2026-10-30 实测）：某账号 expiresAt 已于 2026-10-27 过期，
// 当天 POST /api/account/resource_detail 仍返回 200（remain=17518）。
// 说明「14 天有效期」只是客户端侧估算，真实失效只能靠 401 兜底。
func loomyExpirySentinel() int64 { return time.Now().AddDate(50, 0, 0).Unix() }

// loomyExpiryIsFarFuture 判断到期时刻是否已是远期哨兵（> 10 年）。
// 留一个宽的判定区间，避免哨兵值本身随时间漂移后被反复改写。
func loomyExpiryIsFarFuture(expiresAt int64) bool {
	return expiresAt > time.Now().AddDate(10, 0, 0).Unix()
}

// healLoomyExpiry 启动自愈：把历史版本按「updatedAt + 14 天」写下的 Loomy
// 到期时刻一次性归一为远期哨兵。
//
// 幂等：已是哨兵的文件不改写；解析不了的文件跳过并留一行日志（不阻断启动）。
// 只在运行期改动 auth.expiresAt 一个字段，其余字段（含已加密的 token）原样保留。
func (a *App) healLoomyExpiry() {
	files, err := filepath.Glob(filepath.Join(a.cfg.AuthDir, "loomy-*.json"))
	if err != nil || len(files) == 0 {
		return
	}
	healed := 0
	for _, file := range files {
		patched, perr := healLoomyFile(file)
		if perr != nil {
			log.Printf("启动自愈：Loomy 凭据到期时刻归一失败（%s）：%v", filepath.Base(file), perr)
			continue
		}
		if patched {
			healed++
		}
	}
	if healed == 0 {
		return
	}
	a.reloadAccounts()
	log.Printf("启动自愈：%d 份 Loomy 凭据的到期时刻已归一为远期哨兵（上游无 refresh 端点，避免恒真刷新）", healed)
}

// healLoomyFile 处理单个 Loomy 凭据文件，返回是否发生了改写。
func healLoomyFile(file string) (bool, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return false, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false, fmt.Errorf("解析失败：%w", err)
	}
	authSection, _ := doc["auth"].(map[string]any)
	if authSection == nil {
		return false, fmt.Errorf("缺少 auth 段")
	}
	// JSON 数字统一解成 float64；缺失/非数字一律当作需要修复。
	expiresAt, _ := authSection["expiresAt"].(float64)
	if loomyExpiryIsFarFuture(int64(expiresAt)) {
		return false, nil
	}
	authSection["expiresAt"] = float64(loomyExpirySentinel())
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false, err
	}
	out = append(out, '\n')
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, file); err != nil {
		// 改名失败时删掉临时文件，避免残留半成品。
		_ = os.Remove(tmp)
		return false, err
	}
	return true, nil
}
