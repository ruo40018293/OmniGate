package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeLoomyAuth 写一份最小可用的 Loomy 凭据文件（字段与真实文件同构）。
func writeLoomyAuth(t *testing.T, dir, uid string, expiresAt int64) string {
	t.Helper()
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  "secret-session-token",
			"refreshToken": "",
			"expiresAt":    expiresAt,
			"domain":       "/api/v1",
			"apiHost":      "https://loomyad.xunfei.cn",
		},
		"account": map[string]any{"uid": uid},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	file := filepath.Join(dir, "loomy-"+uid+".json")
	if err := os.WriteFile(file, append(raw, '\n'), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return file
}

func readLoomyAuth(t *testing.T, file string) (int64, map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	auth, _ := doc["auth"].(map[string]any)
	exp, _ := auth["expiresAt"].(float64)
	return int64(exp), auth
}

// R94：历史版本按「updatedAt + 14 天」写下的到期时刻必须被归一为远期哨兵。
//
// 不归一的话，auth.NeedsRefreshLocked（只看 ExpiresAt、不看 refreshToken）
// 会在第 15 天起恒真 → loomy.RefreshToken 恒返回 ErrSessionDead → 面板红字
// 「token refresh failed」+ 该号被跳过 + 每 10 分钟一次必然失败的刷新。
func TestHealLoomyExpiryNormalisesLegacyExpiry(t *testing.T) {
	dir := t.TempDir()
	legacy := time.Now().Add(14 * 24 * time.Hour).Unix() // 旧写法：updatedAt + 14d
	file := writeLoomyAuth(t, dir, "u1", legacy)

	patched, err := healLoomyFile(file)
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if !patched {
		t.Fatal("14 天到期时刻必须被改写为远期哨兵")
	}
	exp, auth := readLoomyAuth(t, file)
	if !loomyExpiryIsFarFuture(exp) {
		t.Fatalf("改写后必须是远期哨兵，实际 %d", exp)
	}
	// 只动 expiresAt，其余字段必须原样保留（token 是加密串，重写会毁掉凭据）。
	if auth["accessToken"] != "secret-session-token" {
		t.Fatalf("accessToken 不得改动，实际 %#v", auth["accessToken"])
	}
	if auth["domain"] != "/api/v1" || auth["apiHost"] != "https://loomyad.xunfei.cn" {
		t.Fatalf("其余字段不得改动，实际 %#v", auth)
	}
}

// 幂等：已经是哨兵的文件不改写（否则每次启动都重写一轮凭据文件）。
func TestHealLoomyExpiryIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	file := writeLoomyAuth(t, dir, "u2", time.Now().AddDate(50, 0, 0).Unix())
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	patched, err := healLoomyFile(file)
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if patched {
		t.Fatal("已是远期哨兵时不得改写")
	}
	after, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("幂等失效：文件内容被改动了")
	}
}

// 到期时刻缺失/为 0（ExpiresAt<=0 会让 NeedsRefresh 恒真）同样要归一。
func TestHealLoomyExpiryFixesMissingExpiry(t *testing.T) {
	dir := t.TempDir()
	file := writeLoomyAuth(t, dir, "u3", 0)

	patched, err := healLoomyFile(file)
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if !patched {
		t.Fatal("expiresAt 为 0 时必须改写")
	}
	exp, _ := readLoomyAuth(t, file)
	if !loomyExpiryIsFarFuture(exp) {
		t.Fatalf("改写后必须是远期哨兵，实际 %d", exp)
	}
}

// 坏文件不能让启动崩掉：解析失败要返回错误而不是 panic，且文件保持原样。
func TestHealLoomyFileRejectsBrokenJSON(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "loomy-broken.json")
	if err := os.WriteFile(file, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	patched, err := healLoomyFile(file)
	if err == nil {
		t.Fatal("坏 JSON 必须返回错误")
	}
	if patched {
		t.Fatal("解析失败时不得报告已改写")
	}
	raw, _ := os.ReadFile(file)
	if string(raw) != "{not json" {
		t.Fatal("解析失败时不得改动原文件")
	}
}
