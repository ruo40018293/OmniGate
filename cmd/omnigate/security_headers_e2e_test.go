package main

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"

	"omnigate/internal/server"
)

// TestWebUIHeadersEndToEnd 用**真实的 embed 前端资源**验证安全头。
//
// 为什么必须有这个测试：wantsHTML 是按路径判定的（见 security_headers.go），
// 而判定逻辑与 embed 资源的真实文件名耦合。若日后把 index.html 改名、
// 或加上 .html 后缀的资源，这里会先炸 —— 否则只有用户打开面板时才发现
// CSP 没生效（或者反过来，该拦的没拦）。
func TestWebUIHeadersEndToEnd(t *testing.T) {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}

	// 真实装配：Handler 带 WebUI，外面包安全头中间件（与 main.go 同构）。
	h := server.SecurityHeaders(server.NewHandler(server.Config{WebUI: sub}), nil)

	cases := []struct {
		path     string
		wantCSP  bool
		wantCode int // 0 表示期望 200
	}{
		{path: "/", wantCSP: true},
		{path: "/app.js", wantCSP: false},
		{path: "/style.css", wantCSP: false},
		{path: "/logo64.png", wantCSP: false},
		// /index.html → FileServer 会 301 到 /（Go 标准行为），
		// 但 CSP 仍应挂在重定向响应上，否则重定向那一跳是裸奔的。
		{path: "/index.html", wantCSP: true, wantCode: 301},
	}

	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", c.path, nil))

		wantCode := c.wantCode
		if wantCode == 0 {
			wantCode = http.StatusOK
		}
		if rec.Code != wantCode {
			t.Errorf("GET %s → %d，want %d", c.path, rec.Code, wantCode)
			continue
		}
		gotCSP := rec.Header().Get("Content-Security-Policy") != ""
		if gotCSP != c.wantCSP {
			t.Errorf("GET %s 带 CSP = %v，want %v", c.path, gotCSP, c.wantCSP)
		}
		if c.wantCSP && rec.Header().Get("X-Frame-Options") != "DENY" {
			t.Errorf("GET %s 缺少 X-Frame-Options: DENY", c.path)
		}
	}
}

// TestIndexHTMLStillRenders 确认 CSP 之外**没有**误设的响应头影响页面渲染：
// Content-Type 必须仍是 text/html，否则浏览器会把 HTML 当纯文本显示。
func TestIndexHTMLStillRenders(t *testing.T) {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	h := server.SecurityHeaders(server.NewHandler(server.Config{WebUI: sub}), nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	ct := rec.Header().Get("Content-Type")
	if ct == "" || ct[:len("text/html")] != "text/html" {
		t.Errorf("Content-Type = %q，want text/html...", ct)
	}
	// no-cache 是既有行为：embed 资源随二进制更新，不加会导致界面与后端版本不匹配。
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q，want no-cache", got)
	}
}
