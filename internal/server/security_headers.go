package server

import (
	"net/http"
	"strings"
	"time"
)

// 安全响应头。
//
// ## 为什么放在最外层而不是 handler.ServeHTTP
//
// 内层 Handler 只接管 /v1/chat/completions、/v1/models、/status、/healthz 与 Web UI，
// 而 /v1/responses、/v1/messages、/v1/messages/count_tokens 由外层兼容层
// （main.go 里的 mux）直接注册。挂在内层会漏掉这四个端点，故中间件在 main.go
// 里包住整个 mux。
//
// ## CSP 的强度说明（重要，不要误以为这是完整 XSS 防护）
//
// 本项目的面板前端存在两类与严格 CSP 冲突的写法：
//
//  1. index.html 从 https://registry.npmmirror.com 加载 echarts（离线环境会失败，
//     但这是既有行为，不在本次改动范围内）；
//  2. 前端有 57 处内联 style 属性、16 处 element.style 赋值，以及 <img onerror=...>
//     这类内联事件属性。
//
// 内联样式属性（style="..."）受 style-src 管辖，内联事件属性（onerror=...）受
// script-src 管辖。要让它们继续工作就必须放开 'unsafe-inline'，而一旦放开，
// script-src 的 XSS 防护基本形同虚设 —— 因为注入 <script> 与注入 <img onerror=...>
// 走的是同一授权。
//
// 因此这里的 CSP 是**有意削弱**的，实际能兜住的只有三类：
//
//   - frame-ancestors 'none'  → 防点击劫持（面板内会渲染上游返回的文本，
//     若可被 iframe 嵌套即可做 UI  redress 攻击）。这是本项目最实际的一条。
//   - object-src / base-uri 'none' → 掐掉 <object>/<base> 注入面。
//   - form-action 'self' → 阻止表单被劫持提交到外域。
//
// 想收紧 CSP 需要先把前端改成「无内联样式 + 无内联事件 + 本地 echarts」，
// 那是一次独立的前端改造，不应与加响应头混在一起做。

// cspPolicy 是默认策略。
//
// connect-src 必须含 https://registry.npmmirror.com：面板的积分趋势图走
// fetch 拉 npmmirror 上的 echarts 运行时。img-src 放开 data: 是因为账号头像
// 与内嵌 logo 存在 data URI 形态。
const cspPolicy = "default-src 'self'; " +
	"script-src 'self' 'unsafe-inline' https://registry.npmmirror.com; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"font-src 'self' data:; " +
	"connect-src 'self' https://registry.npmmirror.com; " +
	"object-src 'none'; " +
	"base-uri 'none'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

// SecurityHeaders 返回包一层安全响应头与访问日志的中间件。
//
// logf 非 nil 时会对每个请求打一行访问日志（耗时 + 状态码 + 字节数），
// 与 logRoundTripper 的出站日志配对，构成一条完整的请求轨迹。
// 生产装配传 log.Printf；测试传 nil 保持安静。
func SecurityHeaders(next http.Handler, logf func(format string, args ...any)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		// 这台程序默认监听环回；若用户改成局域网监听并设了 API-Key，
		// 浏览器直连聊天页会把该 origin 暴露给 Referer。
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		// 面板与 API 同源，无需跨源读取；显式关闭可挡掉「若被嵌套则读取响应」
		// 这一类攻击面。
		h.Set("Cross-Origin-Resource-Policy", "same-origin")

		// CSP 只对 HTML 文档有意义：给 /v1/chat/completions 的 JSON 响应挂 CSP
		// 既无作用，又会让 curl 调试时看到一堆无关头。仅在响应确实是 HTML 时设置。
		if wantsHTML(r) {
			h.Set("Content-Security-Policy", cspPolicy)
		}

		if logf == nil {
			next.ServeHTTP(w, r)
			return
		}
		// 记录访问日志：包一层 ResponseWriter 捕获状态码与字节数。
		// 只记元数据（路径/状态/耗时/大小），**不记请求体与响应体** ——
		// 里面含提示词与模型输出。
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		dur := time.Since(start)
		if quietPoll(r, rec.status, dur) {
			return
		}
		logf("access %s %s status=%d bytes=%d dur=%s", r.Method, r.URL.Path,
			rec.status, rec.bytes, dur.Round(time.Millisecond))
	})
}

// quietPoll 判断这条访问日志是否属于「面板自动轮询」因而可以丢弃（R73）。
//
// 背景（17 天日志实测）：93,001 条 access 里 78,839 条（84.8%）是面板自己的
// /api/* GET，其中 /api/state 一条就占 32,250 条。它们全是 200、耗时 <0.1s，
// 既不能定位故障也不能复盘用户动作，却把日志写成 8.5K-18.8K 行/天，
// 把真正该看的签到/失败/上游错误淹没。
//
// 判据取「GET + /api/ 前缀 + 2xx + 快」四条同时成立：
//   - POST /api/* 是用户动作（登录、签到、刷新、换号），必须留痕；
//   - 非 2xx 是异常，必须留痕（这条保证上面的省量不会吃掉故障证据）；
//   - 慢的轮询（>500ms）留痕，那是面板卡顿的第一手线索。
//
// 想复盘轮询量本身，看 /api/stats 的请求计数与前端 fetch 的 console 即可。
func quietPoll(r *http.Request, status int, dur time.Duration) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		return false
	}
	if status < 200 || status >= 300 {
		return false
	}
	return dur < 500*time.Millisecond
}

// wantsHTML 判断本次请求是否可能返回一个 HTML 文档。
//
// 按「请求路径」而非「响应 Content-Type」判断，是因为此时还没调用下游，
// 拿不到 Content-Type；而路径前缀足以精确圈定静态页面：面板只有根路径
// 与 SPA 内的 hash 路由（hash 不进 HTTP 请求，故只有 "/" 一种形态），
// 其余全是 /v1/、/api/、/status、/healthz。
func wantsHTML(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	p := r.URL.Path
	// 静态资源显式排除：这些路径虽然也是 GET，但返回的是 js/css/png，
	// 挂 CSP 只会污染浏览器的 devtools 视图。
	if strings.HasPrefix(p, "/v1/") || strings.HasPrefix(p, "/api/") ||
		p == "/status" || p == "/healthz" || strings.HasSuffix(p, ".js") ||
		strings.HasSuffix(p, ".css") || strings.HasSuffix(p, ".png") ||
		strings.HasSuffix(p, ".ico") || strings.HasSuffix(p, ".svg") ||
		strings.HasSuffix(p, ".json") {
		return false
	}
	return true
}

// statusRecorder 捕获状态码与已写字节数，用于访问日志。
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wrote = true
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Flush 透传给下游，保证 SSE 流式响应不被本层缓冲。
// ChatCompletions 走 http.Flusher，缺这个方法会导致 SSE 退化为一次性返回。
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
