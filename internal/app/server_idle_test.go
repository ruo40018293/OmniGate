package app

import (
	"net/http"
	"testing"
	"time"
)

// TestNewHTTPServerTimeouts 回归 R47：主 HTTP 服务必须显式设置 IdleTimeout。
//
// 背景（2026-10-21 生产）：DSH 经由本网关的请求出现「发出去了但网关零日志」，
// 客户端无限等待、重启网关即恢复；同一时刻换官方模型正常（走另一套 host/连接池）
// ⇒ 指向客户端复用到了一条对端已关闭 / half-open 的 keep-alive 连接。
// 窗口期 17:38:41–17:52:08 共 13 分 27 秒，网关只有 /api/state 轮询、无任何业务请求。
//
// 根因：Go 的 http.Server 在 IdleTimeout == 0 时回退到 ReadTimeout，
// 而本服务两者都没设 ⇒ keep-alive 连接永不超时，何时关闭完全由客户端决定。
func TestNewHTTPServerTimeouts(t *testing.T) {
	srv := newHTTPServer(http.NewServeMux())
	if srv.IdleTimeout <= 0 {
		t.Fatalf("IdleTimeout = %v，必须 > 0：不设置会让 keep-alive 连接永不超时（R47）", srv.IdleTimeout)
	}
	// 取值约束：不能短于常见单次请求耗时（实测 2–9s，流式更长），否则会误杀活跃连接；
	// 也不能长到失去回收意义。
	if srv.IdleTimeout < 30*time.Second || srv.IdleTimeout > 5*time.Minute {
		t.Fatalf("IdleTimeout = %v，应在 [30s, 5m]：过短会误杀长回答，过长则半死连接回收失效", srv.IdleTimeout)
	}
	// ReadHeaderTimeout 必须保留：防止慢速 header 攻击把连接挂住。
	if srv.ReadHeaderTimeout <= 0 {
		t.Fatalf("ReadHeaderTimeout = %v，必须 > 0", srv.ReadHeaderTimeout)
	}
	// Handler 必须透传，否则抽函数时容易漏接线。
	if srv.Handler == nil {
		t.Fatal("Handler 为 nil：newHTTPServer 必须透传 handler")
	}
}
