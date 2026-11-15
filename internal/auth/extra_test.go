package auth

// R104 Extra 字段的读写往返闸门。
//
// 背景：`auth.Auth` 原本没有承载渠道扩展凭据的位置。zcode 需要 device_mid /
// app_version，codearts 需要 STS 的 AK/SK/security_token，officeace 需要
// dpop_priv —— 这些既不能塞进 AccessToken（语义不同），也不能各建一个渠道私有
// 结构（`Parse` 是唯一读盘入口，私有的解析必然漏掉 RefreshToken 轮换后的落盘）。
//
// 加了 Extra 之后有三个必须钉死的性质：
//  1. 落盘-读回**不丢字段**（refresh 之后 SaveAtomic 会把整份凭据重写，丢一个键
//     就等于账号下次启动失效，而且是「上游 400 code 3001」这种毫无线索的报错）；
//  2. 空 Extra 不得写出 `"extra": {}`——否则所有老凭证文件都会被改写一遍；
//  3. 老凭证（无 extra 字段）必须能正常解析，Extra 为 nil。
//
// 三条都被变异验证覆盖（见 docs/DECISIONS.md R104）。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtraSurvivesSaveLoadRoundtrip 写盘再读回，Extra 的每个键都必须原样在。
func TestExtraSurvivesSaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "zcode-u1.json")
	extra := map[string]string{
		"device_mid":  "mid-abc-123",
		"app_version": "1.2.3",
		"access_key":  "AKIDEXAMPLE",
	}
	a := &Auth{
		Kind:        "zcode",
		AccessToken: "tok",
		ExpiresAt:   4102444800,
		UID:         "u1",
		Extra:       extra,
		FilePath:    path,
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got.Extra) != len(extra) {
		t.Fatalf("Extra 键数 %d，预期 %d（实际 %v）", len(got.Extra), len(extra), got.Extra)
	}
	for k, want := range extra {
		if got.Extra[k] != want {
			t.Errorf("Extra[%q] = %q，预期 %q", k, got.Extra[k], want)
		}
	}
}

// TestEmptyExtraIsOmittedFromDisk 空 Extra 不得在盘上留下 `"extra": {}`。
//
// 为什么较真：写入端每条凭证都会 SaveAtomic，多一个空对象就意味着一份无意义的
// diff（所有老账号文件全被改写），也会让「哪个账号真的带扩展凭据」变得看不出来。
func TestEmptyExtraIsOmittedFromDisk(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra map[string]string
	}{
		{"nil", nil},
		{"空 map", map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wb-u2.json")
			a := &Auth{
				Kind:        "workbuddy",
				AccessToken: "tok",
				ExpiresAt:   4102444800,
				UID:         "u2",
				Extra:       tc.extra,
				FilePath:    path,
			}
			if err := a.SaveAtomic(); err != nil {
				t.Fatalf("SaveAtomic: %v", err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if strings.Contains(string(raw), `"extra"`) {
				t.Errorf("空 Extra 不该写到盘上，但文件里有 extra 字段：%s", raw)
			}
		})
	}
}

// TestLegacyCredentialWithoutExtraParses 老凭证（无 extra）必须能解析，Extra 为 nil。
// 向后兼容是硬要求：升级后第一次启动会读遍 auths/ 下所有文件。
func TestLegacyCredentialWithoutExtraParses(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"嵌套形", `{"auth":{"accessToken":"tok","expiresAt":4102444800},"account":{"uid":"u3"}}`},
		{"扁平形", `{"accessToken":"tok","expiresAt":4102444800,"uid":"u3"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := Parse([]byte(tc.raw))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if a.Extra != nil {
				t.Errorf("老凭证的 Extra 应为 nil，实际 %v", a.Extra)
			}
			if a.UID != "u3" {
				t.Errorf("UID = %q，预期 u3", a.UID)
			}
		})
	}
}

// TestFlatCredentialKeepsExtra 扁平形凭证的 extra 必须也能读出来。
//
// 为什么单独测：`SaveAtomic` 写的是**嵌套形**，往返测试永远走不到
// 扁平形分支。但 codearts / officeace 的导入凭证可能是扁平形
// (它们不走本仓的 SaveAtomic，而是外部工具写的)——那时
// Extra 丢了就会以为“没有 AK/SK”，然后签名失败且报错看不出原因。
func TestFlatCredentialKeepsExtra(t *testing.T) {
	raw := `{"accessToken":"tok","expiresAt":4102444800,"uid":"u5",
		"extra":{"access_key":"AKIDX","security_token":"STS","region":"cn-north-4"}}`
	a, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := map[string]string{"access_key": "AKIDX", "security_token": "STS", "region": "cn-north-4"}
	if len(a.Extra) != len(want) {
		t.Fatalf("Extra 键数 %d，预期 %d（实际 %v）", len(a.Extra), len(want), a.Extra)
	}
	for k, v := range want {
		if a.Extra[k] != v {
			t.Errorf("Extra[%q] = %q，预期 %q", k, a.Extra[k], v)
		}
	}
}

// TestExtraIsWrittenUnderAuthKey 嵌套形落盘时 extra 必须在 `auth` 段内。
//
// 契约来自 zcode 的参考实现：插件读的是 `auth.extra`。放到顶层读不到，
// 而且不会报错——只是 device_mid 变空，然后 billing 全家桶 400 code 3001。
func TestExtraIsWrittenUnderAuthKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zcode-u4.json")
	a := &Auth{
		Kind:        "zcode",
		AccessToken: "tok",
		ExpiresAt:   4102444800,
		UID:         "u4",
		Extra:       map[string]string{"device_mid": "m"},
		FilePath:    path,
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc struct {
		Auth    map[string]json.RawMessage `json:"auth"`
		Account map[string]json.RawMessage `json:"account"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := doc.Auth["extra"]; !ok {
		t.Errorf("extra 必须写在 auth 段内（插件读 auth.extra）；实际顶层键数=%d auth 键数=%d", len(doc.Auth), len(doc.Account))
	}
}
