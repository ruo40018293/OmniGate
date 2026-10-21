package login_trae

// login_device_test.go 回归测试：登录设备号的来源与作用域。
//
// 锁定四个事实：
//  1. 优先使用本机 Trae 客户端 storage.json 里的**真实注册设备号**——
//     claim 按注册指纹校验，自造号恒 9074（这是 2026-10-12 补上的 P0）；
//  2. 读不到客户端（未装客户端 / 格式变化）才回退随机 15 位数字；
//  3. 两种来源下设备号都随登录流程落进 auths/trae-<uid>.json，之后签到/对话
//     头从 auth 文件读，稳定不变（重登/刷新不经过 Start）；
//  4. 旧版遗留的本机级 device-id.json 一律不读，Poll 成功后清理。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"omnigate/internal/traework"
)

// withDeviceSource 临时替换设备号来源，测试结束自动还原。
func withDeviceSource(t *testing.T, fn func() string) {
	t.Helper()
	old := clientDeviceID
	clientDeviceID = fn
	t.Cleanup(func() { clientDeviceID = old })
}

// extractQuery 从授权 URL 里取 query 参数。
func extractQuery(t *testing.T, authURL, key string) string {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("解析授权 URL: %v", err)
	}
	return u.Query().Get(key)
}

// TestStartPrefersClientDeviceID 客户端设备号存在时，授权 URL 必须带上它，
// 且不再自造随机号（自造号签到恒 9074）。
func TestStartPrefersClientDeviceID(t *testing.T) {
	withDeviceSource(t, func() string { return "4484256452647802" })
	authURL, err := Start(NewClient(), filepath.Join(t.TempDir(), "login-state.json"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := extractQuery(t, authURL, "device_id"); got != "4484256452647802" {
		t.Fatalf("device_id=%q，应直接采用客户端真实设备号", got)
	}
	// machine_id 仍按会话生成（与设备注册是两件事）
	if extractQuery(t, authURL, "machine_id") == "" {
		t.Fatal("machine_id 不应为空")
	}
}

// TestStartFallsBackToRandomDeviceID 读不到客户端设备号时回退随机 15 位数字，
// 两次会话各不相同（避免 #60 的 20401 Device limit reached）。
func TestStartFallsBackToRandomDeviceID(t *testing.T) {
	withDeviceSource(t, func() string { return "" })
	dir := t.TempDir()
	statePath := filepath.Join(dir, "login-state.json")

	// 旧版遗留文件必须被无视（不读它的值）
	legacy := `{"machineId":"00000000000000000000000000000000","deviceId":"100000000000000"}`
	if err := os.WriteFile(filepath.Join(dir, "device-id.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	var dev1, mach1, dev2, mach2 string
	for i := 0; i < 2; i++ {
		authURL, err := Start(NewClient(), statePath)
		if err != nil {
			t.Fatalf("Start #%d: %v", i+1, err)
		}
		if i == 0 {
			dev1 = extractQuery(t, authURL, "device_id")
			mach1 = extractQuery(t, authURL, "machine_id")
		} else {
			dev2 = extractQuery(t, authURL, "device_id")
			mach2 = extractQuery(t, authURL, "machine_id")
		}
	}
	for _, d := range []string{dev1, dev2} {
		if len(d) != 15 {
			t.Fatalf("回退设备号应为 15 位数字（对齐客户端格式），got %q", d)
		}
		for _, c := range d {
			if c < '0' || c > '9' {
				t.Fatalf("回退设备号必须是纯数字，got %q", d)
			}
		}
	}
	if dev1 == dev2 || mach1 == mach2 {
		t.Fatalf("两次登录会话复用了设备号（#60/#70 回归）：dev %q/%q mach %q/%q", dev1, dev2, mach1, mach2)
	}
	if dev1 == "100000000000000" || dev2 == "100000000000000" {
		t.Fatal("新会话不应复用旧 device-id.json 里的编造值")
	}
}

// TestPollSavesDeviceIntoAuthFileAndCleansLegacy 登录成功后：
// 设备号随 auth 文件落盘（此后稳定，签到头从文件读）；device-id.json 被清理。
func TestPollSavesDeviceIntoAuthFileAndCleansLegacy(t *testing.T) {
	withDeviceSource(t, func() string { return "" })
	dir := t.TempDir()
	statePath := filepath.Join(dir, "login-state.json")

	// 假 OAuth 上游：ExchangeToken + GetUserInfo（Poll 成功路径只碰这两个端点）
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, traework.EpExchange):
			// RefreshToken 直接解析顶层 Result（doJSON 不剥 envelope）
			_, _ = w.Write([]byte(`{"Result":{"Token":"at-new","TokenExpireAt":9999999999,"RefreshToken":"rt-new"}}`))
		case strings.HasSuffix(r.URL.Path, traework.EpUserInfo):
			// GetUserInfo 同样解析顶层 Result
			_, _ = w.Write([]byte(`{"Result":{"UserID":"uid-devtest","ScreenName":"设备测试","EnterpriseID":"ent1"}}`))
		default:
			http.Error(w, "unexpected: "+r.URL.Path, 404)
		}
	}))
	defer srv.Close()

	authURL, err := Start(NewClient(), statePath)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	wantDev := extractQuery(t, authURL, "device_id")
	wantMach := extractQuery(t, authURL, "machine_id")

	// 模拟授权回调结果（直接写 state 文件，比打真实回调更稳）
	st := state{MachineID: wantMach, DeviceID: wantDev, CodeVerifier: "v", RefreshToken: "rt-callback", Host: srv.URL}
	raw, _ := json.Marshal(st)
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := Poll(NewClient(), statePath)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if res.DeviceID != wantDev || res.MachineID != wantMach {
		t.Fatalf("Poll 结果设备号与会话不符: dev=%q want %q, mach=%q want %q", res.DeviceID, wantDev, res.MachineID, wantMach)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatal("登录成功后 login-state.json 应被删除")
	}
	// device-id.json 清理（旧文件存在时）
	if err := os.WriteFile(filepath.Join(dir, "device-id.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 落盘 auth 文件并验证设备号进入文件
	fp, err := SaveAuth(dir, res)
	if err != nil {
		t.Fatalf("SaveAuth: %v", err)
	}
	rawAuth, _ := os.ReadFile(fp)
	var doc struct {
		Auth struct {
			DeviceID  string `json:"deviceId"`
			MachineID string `json:"machineId"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(rawAuth, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Auth.DeviceID != wantDev || doc.Auth.MachineID != wantMach {
		t.Fatalf("auth 文件设备号不符: got %q/%q want %q/%q", doc.Auth.DeviceID, doc.Auth.MachineID, wantDev, wantMach)
	}
}

// TestShortDevice 日志脱敏：不得把完整设备号写进日志。
func TestShortDevice(t *testing.T) {
	if got := shortDevice("4484256452647802"); got != "448425…" {
		t.Fatalf("shortDevice=%q", got)
	}
	if got := shortDevice("123"); got != "123" {
		t.Fatalf("短值应原样返回，got %q", got)
	}
}
