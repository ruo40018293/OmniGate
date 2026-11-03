// sse.go 处理千问办公嵌套 SSE：每行 data:{"headers":{...},"body":"<json-string>",
// "statusCodeValue":200,"statusCode":"OK"}，body 字段需二次解析得到标准 OpenAI chunk。
//
// 关键差异：网关对错误（如缺 request_id）也返回 HTTP 200，
// 错误信息包在 envelope 的 statusCodeValue>=400 里 —— 必须剥壳判错，
// 不能只看 HTTP 状态码（Buddy2api 的 envelope_error() 即为此设计）。
package qwenwork

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"omnigate/internal/provider"
)

// sseError 上游以 HTTP 200 包裹的业务失败（R90）。
//
// 与 internal/qodercn 的同名类型职责一致：把「流内业务错误」变成可分类 error，
// 让 handler 能用 errors.As 拿到 kind 并据此冷却/换号。两边不共用一份实现，
// 是因为各自的 Classify 归并口径不同（qwenwork 的 402 词表比 qodercn 宽）。
type sseError struct {
	status int
	msg    string
}

func (e *sseError) Error() string { return fmt.Sprintf("upstream %d: %s", e.status, e.msg) }

// Kind 实现 provider.StreamErrorClassifier（复用 HTTP 路径同一套 Classify）。
func (e *sseError) Kind() provider.ErrKind { return Classify(e.status, e.msg) }

var _ provider.StreamErrorClassifier = (*sseError)(nil)

// writeStreamErrorFrame 补写一帧带原因的 SSE error（**不补 [DONE]**，R36）。
func writeStreamErrorFrame(w io.Writer, e *sseError) {
	payload, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": truncate(e.msg, 300),
		"type":    "upstream_error",
		"code":    streamErrorCode(e.Kind()),
	}})
	if _, werr := w.Write(append(append([]byte("data: "), payload...), []byte("\n\n")...)); werr != nil {
		return
	}
	if fl, ok := w.(interface{ Flush() }); ok {
		fl.Flush()
	}
}

// streamErrorCode 把 ErrKind 归一成客户端可见的 code（只需覆盖本渠道会出现的几类）。
func streamErrorCode(k provider.ErrKind) string {
	switch k {
	case provider.ErrHardCredit:
		return "upstream_quota_exhausted"
	case provider.ErrSoftRate:
		return "upstream_rate_limited"
	case provider.ErrSessionDead:
		return "upstream_session_dead"
	default:
		return "upstream_business_error"
	}
}

// envelope SSE 外层信封。
type envelope struct {
	Headers         map[string]any  `json:"headers"`
	Body            json.RawMessage `json:"body"` // 可能是字符串（内层 JSON 文本）或对象
	StatusCodeValue int             `json:"statusCodeValue"`
	StatusCode      string          `json:"statusCode"`
}

// errText 从 envelope 提取错误文案；非错误返回空串。
func (e *envelope) errText() string {
	if e.StatusCodeValue < 400 {
		return ""
	}
	// body 可能是 {"code":"400","message":"..."} 或纯字符串
	var withMsg struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	if err := json.Unmarshal(e.Body, &withMsg); err == nil && withMsg.Message != "" {
		return withMsg.Message
	}
	return truncate(string(e.Body), 200)
}

// innerChunk 解出内层 OpenAI chunk；无有效内容（[DONE]/{}）返回 nil。
func (e *envelope) innerChunk() (map[string]any, error) {
	if len(e.Body) == 0 {
		return nil, nil
	}
	// body 为字符串形式（最常见）：先解字符串再解 JSON
	var s string
	if err := json.Unmarshal(e.Body, &s); err == nil {
		t := strings.TrimSpace(s)
		if t == "" || t == "[DONE]" || t == "{}" {
			return nil, nil
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(t), &chunk); err != nil {
			return nil, nil // 跳过无法解析的行（不致命）
		}
		return chunk, nil
	}
	// body 为对象形式
	var chunk map[string]any
	if err := json.Unmarshal(e.Body, &chunk); err != nil {
		return nil, nil
	}
	if len(chunk) == 0 {
		return nil, nil
	}
	return chunk, nil
}

// parseNestedSSE 逐行解析嵌套 SSE：
//   - envelope statusCodeValue >= 400 → 返回错误（上游错误以 HTTP 200 包裹）
//   - body == "[DONE]"/"{}" → 正常结束
//   - body 二次解析失败 → 跳过该行（不致命）
//
// truncated 为出参（调用方给非 nil 指针才写）：报告「可证实的截断」——传输层错误
// （连接中断 / 读超时）或 EOF 处停在一个不完整的帧上。调用方据此决定是向客户端补
// [DONE] 还是改发 error 帧：把半截流伪装成正常结束，会让客户端拿着残缺的 tool_call
// arguments 去解析并报 "tool input was not fully received"（issue #42）。
//
// 注意：本渠道的 [DONE] 在 envelope 内部（body=="[DONE]"），**不是**客户端可见的
// data: [DONE] 帧，故它只用于停止解析，不能拿来抑制调用方的收尾帧。
func parseNestedSSE(r io.Reader, onChunk func(map[string]any) error, truncated *bool) error {
	set := func() {
		if truncated != nil {
			*truncated = true
		}
	}
	br := bufio.NewReaderSize(r, 256*1024)
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			set() // 连接中断 / 读超时：可证实的截断
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimPrefix(line, "data:")
			var env envelope
			outerOK := json.Unmarshal([]byte(payload), &env) == nil
			if outerOK && env.StatusCodeValue != 0 {
				if msg := env.errText(); msg != "" {
					// R90：不再用裸 fmt.Errorf —— 那玩意的类型是实现不了
					// provider.StreamErrorClassifier 的，handler 的 errors.As 取不到
					// kind ⇒ 落 default「不罚账号」⇒ 粘性路由把下一次请求送回同一个
					// 刚撞完额度上限的号，客户端每次都要等 180 秒才看到一句「断了」。
					// 实测生产 118 条 `upstream 402: {"code":"402","message":"Error in
					// upstream response"}` 全部走的就是这条丢信息的路径。
					return &sseError{status: env.StatusCodeValue, msg: msg}
				}
			}
			if outerOK {
				chunk, _ := env.innerChunk()
				if chunk != nil {
					if err := onChunk(chunk); err != nil {
						return err
					}
				} else if innerTruncated(env.Body) {
					// R90 修：innerChunk 对「解不出来」一律返回 nil,
					// 于是「内层是半截 JSON」与「内层是 [DONE]」完全同形，
					// 半帧被当成正常收尾：客户端收到 [DONE]、网关记成一条
					// 成功流水，实际上游只发了一半。
					set()
				}
			} else {
				// R90 修：data: 行的**外层** JSON 就不合法 = 这一帧被切断在半路上。
				//
				// 此前只有「恰好卡在 EOF」时才判截断，于是「上游写到一半断连、但残帧
				// 自带换行」（生产上极常见——残帧后面就跟着两个换行收尾）被判成干净
				// 收尾：客户端收到 [DONE]、网关记成一条成功流水，实际只收到半个帧。
				set()
			}
		}
		if err == io.EOF {
			// 末尾没有换行收尾时，ReadString 会把「最后一个非空帧 + 其中的空行」一起
			// 交回来，故按最后一个**非空行**判定：它是空行 → 干净收尾；
			// 否则帧不完整（半帧）。
			if last := lastNonBlankLine(line); last != "" {
				set()
			}
			return nil
		}
	}
}

// aggregate 聚合完整 SSE 为单个 OpenAI chat.completion 响应。
// model 参数覆盖响应中的 model 字段（上游回显的是档位 key）。
func aggregate(r io.Reader, model string) (map[string]any, error) {
	var (
		id           string
		created      float64
		content      strings.Builder
		reasoning    strings.Builder
		role         = "assistant"
		finishReason = "stop"
		usage        map[string]any
		toolCalls    = map[int]map[string]any{}
		toolOrder    []int
	)
	err := parseNestedSSE(r, func(chunk map[string]any) error {
		if v, ok := chunk["id"].(string); ok && id == "" {
			id = v
		}
		if v, ok := chunk["created"].(float64); ok && created == 0 {
			created = v
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		ch, _ := chunk["choices"].([]any)
		for _, ci := range ch {
			c, _ := ci.(map[string]any)
			if c == nil {
				continue
			}
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				finishReason = fr
			}
			delta, ok := c["delta"].(map[string]any)
			if !ok {
				continue
			}
			if r2, ok := delta["role"].(string); ok && r2 != "" {
				role = r2
			}
			if txt, ok := delta["content"].(string); ok {
				content.WriteString(txt)
			}
			if rc, ok := delta["reasoning_content"].(string); ok {
				reasoning.WriteString(rc)
			}
			if tcs, ok := delta["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					call, ok := tc.(map[string]any)
					if !ok {
						continue
					}
					idx := 0
					if v, ok := call["index"].(float64); ok {
						idx = int(v)
					}
					merged, seen := toolCalls[idx]
					if !seen {
						merged = map[string]any{"index": idx}
						toolCalls[idx] = merged
						toolOrder = append(toolOrder, idx)
					}
					mergeToolCallDelta(merged, call)
				}
			}
		}
		return nil
	}, nil)
	if err != nil {
		return nil, err
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	message := map[string]any{
		"role":    role,
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sortInts(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}

// mergeToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖，function.arguments 拼接。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

// sortInts 升序排序。
func sortInts(a []int) {
	for i := 0; i < len(a)-1; i++ {
		for j := i + 1; j < len(a); j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}

// streamAsOpenAI 把嵌套 SSE 流边读边转写为标准 OpenAI SSE 给客户端。
// 每个 chunk 重写 model 字段为客户端模型名。
//
// 收尾规则（issue #42）：正常收尾（含上游未发 [DONE] 的兜底）补 data: [DONE]；
// 可证实的截断（传输层错误 / 停在半个帧上）改发一帧 OpenAI 规范 error 且**不补**
// [DONE]，让客户端明确感知失败，而不是收到半截 tool_call 参数。
func streamAsOpenAI(w io.Writer, r io.Reader, model string, flush func()) (map[string]any, error) {
	var usage map[string]any
	truncated := false
	err := parseNestedSSE(r, func(chunk map[string]any) error {
		// usage 捕获：末帧覆盖前面（OpenAI 语义末帧才是全量），并照常透传
		if u, ok := chunk["usage"].(map[string]any); ok && len(u) > 0 {
			usage = u
		}
		chunk["model"] = model
		raw, _ := json.Marshal(chunk)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return err
		}
		if flush != nil {
			flush()
		}
		return nil
	}, &truncated)
	// 截断优先于 err：读错误（连接中断/超时）本身就体现为 err != nil，必须先把
	// 已透传的半截流的收尾处理掉（发 error 帧、不补 [DONE]），再报错给调用方。
	if truncated {
		// R90：出口帧与日志都保留**原始原因**。此前无论什么原因都只写固定串
		// "upstream stream truncated" 并把 err 丢弃 ⇒ 生产上分不清「空闲看门狗掐的」
		// （连续无字节，疑似上游排队 / 额度耗尽挂死）、「上游主动断连」、
		// 「读到半个帧就 EOF」这三种完全不同的故障。用户看到的「只知道断了」
		// 就是这个丢弃的直接后果。
		//
		// 空闲超时另给 code=upstream_timeout，其余仍为 upstream_truncated ——
		// 对外 code 集合不变，internal/gateway 的 streamErrKind 分类不受影响。
		if werr := provider.WriteTruncationFrame(w, err); werr != nil {
			return usage, werr
		}
		if flush != nil {
			flush()
		}
		return usage, provider.TruncationError(err)
	}
	// R90：流内业务错误（信封 statusCodeValue>=400）已归一成 *sseError。
	// 补一帧带原因的 SSE error 并**不补 [DONE]**（R36）；同时把错误上抛，
	// 让 handler 按 Kind() 冷却账号 —— 这是「额度耗尽后下一次请求自动换号」的唯一
	// 实现路径（流式响应头早已发出，同一请求内没法换号重试）。
	if sErr := (*sseError)(nil); errors.As(err, &sErr) {
		writeStreamErrorFrame(w, sErr)
		if flush != nil {
			flush()
		}
		return usage, sErr
	}
	if err != nil {
		return usage, err
	}
	// 正常收尾（含上游未发 [DONE] 的兜底）：保证恰好一个 data: [DONE]。
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return usage, err
	}
	if flush != nil {
		flush()
	}
	return usage, nil
}

// Stream 实现 provider.Upstream：嵌套 SSE → 标准 OpenAI SSE 透传。
// 返回值为末帧捕获的 usage（供记账，上游未返回时为 nil）。
func Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	return StreamCapture(w, r, model, nil)
}

// StreamCapture 同 Stream，额外把末帧 usage 回调给 onUsage（非 nil 时）。
func StreamCapture(w http.ResponseWriter, r io.Reader, model string, onUsage func(map[string]any)) (map[string]any, error) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	flush := func() {
		if fl != nil {
			fl.Flush()
		}
	}
	usage, err := streamAsOpenAI(w, r, model, flush)
	if err == nil && onUsage != nil && usage != nil {
		onUsage(usage)
	}
	return usage, err
}

// truncate 字符串截断（rune 安全）。
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}

// lastNonBlankLine 返回多行文本里最后一个非空行（去空白）。
//
// 用于 EOF 收尾判定：ReadString('\n') 在最后一段没有换行收尾时，会把「最后一个
// 完整帧 + 它自带的空行」一并交回，此时行内仍含换行，
// 直接 TrimSpace 判不出「末行其实是空的」。
func lastNonBlankLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// innerTruncated 报告 envelope 里的内层 body 是不是一个被切断的半帧。
//
// 为什么需要它：innerChunk 对所有「解不出 chunk」的情形都返回 nil（包括
// [DONE]、{}、空串、以及**半截的 JSON**），判不出哪些是正常收尾、
// 哪些是上游断在半路。这里按「非空且不是 [DONE]/{} 但 JSON 不合法」定义半帧。
func innerTruncated(body json.RawMessage) bool {
	if len(body) == 0 {
		return false
	}
	var s string
	if err := json.Unmarshal(body, &s); err == nil {
		t := strings.TrimSpace(s)
		if t == "" || t == "[DONE]" || t == "{}" {
			return false
		}
		return !json.Valid([]byte(t))
	}
	return !json.Valid(body)
}
