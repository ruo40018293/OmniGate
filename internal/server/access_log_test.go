package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestAccessLogQuietPoll 面板自动轮询不写访问日志，其余一律留痕（R73）。
//
// 丢弃判据是「GET + /api/ 前缀 + 2xx + 快」四条同时成立。这里逐条反证：
// 换 POST、换 /v1/、状态码 5xx、耗时拖到 600ms，任意一条不满足都必须留痕——
// 「省日志」的前提是绝不能把故障证据一起吃进肚子。
func TestAccessLogQuietPoll(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		path    string
		status  int
		delay   time.Duration
		wantLog bool
	}{
		{"轮询正常 丢弃", "GET", "/api/state", 200, 0, false},
		{"轮询慢 留痕", "GET", "/api/state", 200, 600 * time.Millisecond, true},
		{"轮询报错 留痕", "GET", "/api/state", 500, 0, true},
		{"用户动作 POST 留痕", "POST", "/api/account/checkin_all", 200, 0, true},
		{"推理流量 留痕", "GET", "/v1/models", 200, 0, true},
		{"静态页面 留痕", "GET", "/", 200, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs []string
			h := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.delay > 0 {
					time.Sleep(tc.delay)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte("ok"))
			}), func(format string, args ...any) {
				logs = append(logs, fmt.Sprintf(format, args...))
			})

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))

			var access []string
			for _, l := range logs {
				if strings.HasPrefix(l, "access ") {
					access = append(access, l)
				}
			}
			if tc.wantLog && len(access) == 0 {
				t.Fatalf("这条访问必须留痕，实际被丢弃（logs=%v）", logs)
			}
			if !tc.wantLog && len(access) > 0 {
				t.Fatalf("面板轮询应被丢弃，实际写了 %v", access)
			}
			if tc.wantLog {
				// 留痕的行必须带状态码与耗时，否则复盘时看不出慢在哪
				if !strings.Contains(access[0], fmt.Sprintf("status=%d", tc.status)) {
					t.Fatalf("访问日志缺状态码：%q", access[0])
				}
				if !strings.Contains(access[0], "dur=") {
					t.Fatalf("访问日志缺耗时：%q", access[0])
				}
			}
		})
	}
}

// TestAccessLogQuietPollThreshold 500ms 是判据门槛：499ms 丢、500ms 留。
// 钉住边界，避免以后把门槛放宽而不自知地吃掉面板卡顿证据。
func TestAccessLogQuietPollThreshold(t *testing.T) {
	req := func(method, path string) *http.Request { return httptest.NewRequest(method, path, nil) }
	if !quietPoll(req("GET", "/api/x"), 200, 499*time.Millisecond) {
		t.Fatal("499ms 应视为快轮询（丢弃）")
	}
	if quietPoll(req("GET", "/api/x"), 200, 500*time.Millisecond) {
		t.Fatal("500ms 不该再算快轮询（必须留痕）")
	}
	if quietPoll(req("GET", "/v1/models"), 200, 0) {
		t.Fatal("/v1/ 是推理流量，永不丢")
	}
	if quietPoll(req("GET", "/api/state"), 302, 0) {
		t.Fatal("非 2xx 必须留痕")
	}
}
