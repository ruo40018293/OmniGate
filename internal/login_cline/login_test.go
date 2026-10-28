package login_cline

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"omnigate/internal/auth"
)

// fakeKeys 是 GET /api/v1/api-keys 的假上游：返回本 Key 与兄弟 Key 两条记录。
// key_prefix 取 key 前 10 字符（上游实测口径，见 cline.probeKey）。
func fakeKeys(t *testing.T, key string) *httptest.Server {
	t.Helper()
	prefix := key
	if len(prefix) > 10 {
		prefix = prefix[:10]
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 生产路径 = APIBase(/api/v1) + EpAPIKeys(/api-keys)；测试把 Base 指向假上游，故只剩 /api-keys。
		if r.URL.Path != "/api-keys" {
			http.NotFound(w, r)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		// 真实响应形态：data 是**对象**，Key 列表在 data.keys（不是数组）。
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"keys": []map[string]any{
					{"id": "apk-abc123", "key_prefix": prefix, "name": "work"},
					{"id": "apk-def456", "key_prefix": "sk_other00", "name": "phone"},
				},
			},
		})
	}))
}

// TestSaveAuthWritesDecryptableCredential 走完整落盘链路：
// 假上游校验 → 写 cline-<uid>.json → auth.LoadClineDir 能读回同一把 key。
//
// 断言口径（缺一条都算没落盘成功）：
//   - 文件名为 cline-apk-abc123.json（uid 来自上游 api-keys 记录）；
//   - 磁盘上是密文（不含明文 key）——防「以为加密了其实写的是明文」；
//   - LoadClineDir 解出 AccessToken == 原 key（DPAPI 同机同用户可解）；
//   - expiresAt 是远期值（不是 0，否则每个请求都会去「刷新」不存在的 refresh 流程）。
func TestSaveAuthWritesDecryptableCredential(t *testing.T) {
	key := "sk_test_abcdefghijklmnop"
	srv := fakeKeys(t, key)
	defer srv.Close()

	dir := t.TempDir()
	uid, pr, err := saveAuth(dir, key, srv.URL)
	if err != nil {
		t.Fatalf("saveAuth: %v", err)
	}
	if uid != "apk-abc123" {
		t.Fatalf("uid = %q, want apk-abc123", uid)
	}
	if pr == nil || len(pr.SiblingKeys) != 1 || pr.SiblingKeys[0] != "sk_other00" {
		t.Fatalf("sibling keys = %+v, want [sk_other00]", pr)
	}

	fp := filepath.Join(dir, "cline-apk-abc123.json")
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if strings.Contains(string(raw), key) {
		t.Fatalf("凭据以明文落盘（应经 auth.EncryptSecret 加密）")
	}
	if !strings.Contains(string(raw), "dpapi:") {
		t.Fatalf("凭据不是 DPAPI 密文形态：%s", string(raw))
	}

	auths, err := auth.LoadClineDir(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("loaded %d accounts, want 1", len(auths))
	}
	if got := auths[0].AccessTokenValue(); got != key {
		t.Fatalf("AccessToken = %q, want %q（解密失败或写错字段）", got, key)
	}
	if auths[0].ExpiresAt <= 0 {
		t.Fatalf("expiresAt = %d，必须为远期正值", auths[0].ExpiresAt)
	}
}

// TestSaveAuthRejectsNonSkKey 前缀不合法必须立刻失败，且**不留下任何文件**
// （否则脏凭据会被 LoadClineDir 读进号池，表现为账号永远 401）。
func TestSaveAuthRejectsNonSkKey(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := saveAuth(dir, "not-a-key", ""); err == nil {
		t.Fatal("非 sk_ 前缀应报错")
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Fatalf("校验失败仍写了 %d 个文件", len(ents))
	}
}
