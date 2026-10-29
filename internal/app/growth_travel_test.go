package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"omnigate/internal/auth"
	"omnigate/internal/config"
	"omnigate/internal/pool"
	"omnigate/internal/provider"
)

// travelUpstream 在 fakeUpstream 之上实现「猫猫旅行」，用于验证面板手动入口。
//
// 记录调用次数与目的地：目的地必须来自配置（0/越界收敛到 1），
// 这是「手动与自动落到同一处」的判据。
type travelUpstream struct {
	fakeUpstream
	calls   atomic.Int32
	lastLoc atomic.Int32
	msg     string
	fail    error
}

func (t *travelUpstream) GrowthTravelOnce(a *auth.Auth, locationID int) (string, error) {
	t.calls.Add(1)
	t.lastLoc.Store(int32(locationID))
	if t.fail != nil {
		return "", t.fail
	}
	return t.msg, nil
}

// newTravelApp 构造单渠道（WorkBuddy）单账号的面板 App，可指定上游与旅行目的地。
func newTravelApp(t *testing.T, up provider.Upstream, uid string, loc int) *App {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.AuthDir = dir
	cfg.AdminPass = "s3cret-pass"
	cfg.Schedule.GrowthTravelLocation = loc
	p := pool.New(filepath.Join(dir, "state-workbuddy.json"))
	p.Add(&auth.Auth{Kind: "workbuddy", UID: uid, AccessToken: "t", RefreshToken: "r"})
	a, err := New(Options{
		ConfigPath: filepath.Join(dir, "config.json"),
		Config:     cfg,
		Runtimes: map[provider.Kind]*Runtime{
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: p, Upstream: up},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.Close)
	return a
}

// travelLogin 登录面板并返回会话 cookie（新文件自带，避免依赖其它测试的私有辅助）。
func travelLogin(t *testing.T, mux *http.ServeMux) *http.Cookie {
	t.Helper()
	lw := doReq(mux, "POST", "/api/auth/login", `{"password":"s3cret-pass"}`, nil)
	if lw.Code != http.StatusOK {
		t.Fatalf("登录失败: %d %s", lw.Code, lw.Body.String())
	}
	cs := lw.Result().Cookies()
	if len(cs) == 0 {
		t.Fatal("登录未回 cookie")
	}
	return cs[0]
}

// travelResp 解析 POST /api/account/growth_travel 的响应体。
type travelResp struct {
	OK  bool   `json:"ok"`
	Msg string `json:"msg"`
}

func decodeTravel(t *testing.T, body []byte) travelResp {
	t.Helper()
	var r travelResp
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, body)
	}
	return r
}

// TestGrowthTravelEndpointOK 手动执行一次旅行：同步回摘要，目的地取配置值。
func TestGrowthTravelEndpointOK(t *testing.T) {
	up := &travelUpstream{msg: "已派出"}
	a := newTravelApp(t, up, "wb-1", 3)
	mux := panelMux(a)
	ck := travelLogin(t, mux)

	w := doReq(mux, "POST", "/api/account/growth_travel", `{"uid":"wb-1"}`, ck)
	if w.Code != http.StatusOK {
		t.Fatalf("执行旅行应 200，实际 %d %s", w.Code, w.Body.String())
	}
	if r := decodeTravel(t, w.Body.Bytes()); !r.OK || r.Msg != "已派出" {
		t.Errorf("响应应为 ok=true msg=已派出，实际 ok=%t msg=%q", r.OK, r.Msg)
	}
	if got := up.calls.Load(); got != 1 {
		t.Errorf("上游应被调用 1 次，实际 %d", got)
	}
	if got := up.lastLoc.Load(); got != 3 {
		t.Errorf("目的地应取配置的 3，实际 %d", got)
	}
}

// TestGrowthTravelClampsLocation 目的地 0 或越界一律收敛到 1（与自动执行同口径）。
func TestGrowthTravelClampsLocation(t *testing.T) {
	for _, loc := range []int{0, 5, -2} {
		up := &travelUpstream{msg: "在途"}
		a := newTravelApp(t, up, "wb-1", loc)
		mux := panelMux(a)
		ck := travelLogin(t, mux)
		if w := doReq(mux, "POST", "/api/account/growth_travel", `{"uid":"wb-1"}`, ck); w.Code != http.StatusOK {
			t.Fatalf("loc=%d 应 200，实际 %d %s", loc, w.Code, w.Body.String())
		}
		if got := up.lastLoc.Load(); got != 1 {
			t.Errorf("loc=%d 应收敛到 1，实际 %d", loc, got)
		}
	}
}

// TestGrowthTravelUnknownUID 账号不在任何池里：400 + 明确文案，且不碰上游。
func TestGrowthTravelUnknownUID(t *testing.T) {
	up := &travelUpstream{msg: "已派出"}
	a := newTravelApp(t, up, "wb-1", 1)
	mux := panelMux(a)
	ck := travelLogin(t, mux)

	w := doReq(mux, "POST", "/api/account/growth_travel", `{"uid":"nope"}`, ck)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("未知账号应 400，实际 %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "账号不存在") {
		t.Errorf("错误应说明账号不存在，实际 %s", w.Body.String())
	}
	if got := up.calls.Load(); got != 0 {
		t.Errorf("未知账号不得调用上游，实际 %d 次", got)
	}
}

// TestGrowthTravelUnsupportedChannel 渠道没有实现猫猫旅行：400 + 可读原因。
func TestGrowthTravelUnsupportedChannel(t *testing.T) {
	a := newTravelApp(t, &fakeUpstream{remain: 5}, "wb-1", 1)
	mux := panelMux(a)
	ck := travelLogin(t, mux)

	w := doReq(mux, "POST", "/api/account/growth_travel", `{"uid":"wb-1"}`, ck)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("不支持的渠道应 400，实际 %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "不支持猫猫旅行") {
		t.Errorf("错误应说明渠道不支持，实际 %s", w.Body.String())
	}
}

// TestGrowthTravelUpstreamError 上游失败：把真实原因透给用户，不编造成功。
func TestGrowthTravelUpstreamError(t *testing.T) {
	up := &travelUpstream{fail: errors.New("上游 502")}
	a := newTravelApp(t, up, "wb-1", 1)
	mux := panelMux(a)
	ck := travelLogin(t, mux)

	w := doReq(mux, "POST", "/api/account/growth_travel", `{"uid":"wb-1"}`, ck)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("上游失败应 400，实际 %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "上游 502") {
		t.Errorf("应原样透出上游错误，实际 %s", w.Body.String())
	}
}
