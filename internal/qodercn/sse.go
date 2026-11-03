// sse.go 处理 QoderCN 嵌套 SSE：每行 data:{...,"body":"<json-string>"}，
// body 字段需二次解析得到标准 OpenAI chunk。
// 移植自 qoderwork2api internal/upstream/sse.go，含聚合与流式转写。
package qodercn

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"

	"net/http"
	"omnigate/internal/capture"
	"strings"
	"sync"
	"time"

	"omnigate/internal/provider"
)

// sseError 上游以 HTTP 200 包裹的业务失败帧（R90）。
//
// 为什么必须有它：信封 statusCodeValue >= 400 的帧（402 额度耗尽、403 风控、
// 400 参数错…）在旧实现里被 path 成「非错误帧」直接丢弃 —— 客户端既收不到
// choices 也收不到 error，界面停在「正在等待模型…」，直到网关 180s 后判截断。
// 更糟的是即便识别到，裸 fmt.Errorf 也**不实现** StreamErrorClassifier，
// handler 的 errors.As 取不到 kind ⇒ 落 default「不罚账号」⇒ 粘性路由把下一次
// 请求送回同一个号，反复撞额度（实测 qwenwork 118 条 402 全是这个形态）。
//
// Kind 复用与 HTTP 路径同一套 Classify，使 server 层不必 import 本渠道包。
type sseError struct {
	status     int
	msg        string
	retryAfter int // 上游 retryAfterSeconds / waitTime；0 = 上游未声明
}

func (e *sseError) Error() string { return fmt.Sprintf("upstream %d: %s", e.status, e.msg) }

// Kind 实现 provider.StreamErrorClassifier。
func (e *sseError) Kind() provider.ErrKind { return Classify(e.status, e.msg) }

var _ provider.StreamErrorClassifier = (*sseError)(nil)

// writeStreamErrorFrame 补写一帧带原因与重试时长的 SSE error（**不补 [DONE]**，R36）。
//
// 补 [DONE] 会把失败伪装成正常结束，客户端拿着空结果继续走，网关侧还把它记成
// 一条成功流水。原因与退避时长结构化传出，重试交回客户端。
func writeStreamErrorFrame(w io.Writer, e *sseError) {
	errObj := map[string]any{
		"message": truncate(e.msg, 300),
		"type":    "upstream_error",
		"code":    streamErrorCode(e.Kind()),
	}
	if e.retryAfter > 0 {
		errObj["retry_after_seconds"] = e.retryAfter
	}
	frame, merr := json.Marshal(map[string]any{"error": errObj})
	if merr != nil {
		// 纯字符串/整数 map，序列化不会失败；真失败也要保证客户端拿到一帧原因。
		frame = []byte(`{"error":{"message":"upstream business error","type":"upstream_error","code":"upstream_business_error"}}`)
	}
	if _, werr := w.Write(append(append([]byte("data: "), frame...), []byte("\n\n")...)); werr != nil {
		log.Printf("qodercn stream error frame write failed: %v", werr)
		return
	}
	if fl, ok := w.(interface{ Flush() }); ok {
		fl.Flush()
	}
}

// streamErrorCode 把 ErrKind 归一成客户端可见的 code。
//
// 与 internal/gateway/chat_sse.go 的 streamErrKind 口径分工不同：那边判的是
// 「客户端看到的 code 要不要改」，这里判的是「对外 code 叫什么」。两者都只做
// 归并、不新增语义 —— 额度耗尽必须让客户端能识别成「可等可换」而非「重试」。
func streamErrorCode(k provider.ErrKind) string {
	switch k {
	case provider.ErrHardCredit:
		return "upstream_quota_exhausted"
	case provider.ErrSoftRate:
		return "upstream_rate_limited"
	case provider.ErrSessionDead:
		return "upstream_session_dead"
	case provider.ErrModelBlocked:
		return "upstream_model_blocked"
	case provider.ErrPassthrough:
		return "upstream_business_error"
	case provider.ErrServer:
		return "upstream_server_error"
	default:
		return "upstream_business_error"
	}
}

// looksLikeErrorFrame 判断扁平帧是否是需要处理的业务错误帧（R46）。
//
// 为什么需要它：正常 chunk 一定带 choices/delta，排队/限流的错误帧则是
// {code, message} 形态（10405/10605/11101…）。只放行后者，避免把
// {"headers":...} 之类噪声当 chunk 透传给客户端。
func looksLikeErrorFrame(m map[string]any) bool {
	if m == nil {
		return false
	}
	if _, ok := m["choices"]; ok {
		return false // 正常 chunk，交给嵌套路径（扁平形态上游不会这么发）
	}
	if v, ok := m["code"]; ok && v != nil && fmt.Sprint(v) != "" && fmt.Sprint(v) != "0" && fmt.Sprint(v) != "200" {
		return true
	}
	return m["message"] != nil && m["error"] == nil
}

// accountLevelFrameError 把**账号级**扁平错误帧归一成 *sseError（R90）。
//
// 与 R46 的 onChunk 下传契约并存：只有「明确属于账号/额度问题」的扁平帧才归一，
// 其余（计费计数超限 110、业务参数错等）继续原样下传。
//
// 判据顺序很讲究：
//  1. 排队帧（10605 / isQueued）先返回 nil —— 它必须留给 onChunk 走 parseQueueFrame
//     生成 *QueueError（queue_test.go 锁定 queueCount/waitTime 两个字段），
//     归一成 sseError 会把队列信息压成一句 message。
//  2. code 必须是能解析的 >= 400 状态码 —— 110 这种业务计数不是 HTTP 状态，
//     归一只会让 Classify 拿到 0 而落 ErrClient（可轮转），把健康号拖下水。
//  3. message 命中额度/余额词表才归一 —— 上游把额度耗尽既发 402 也发 403/200
//     包 code，词表是唯一可靠的判据。
func accountLevelFrameError(m map[string]any) *sseError {
	msg := ""
	if s, ok := m["message"].(string); ok {
		msg = s
	}
	if looksLikeQueueFrame(msg) {
		return nil
	}
	status := 0
	if v, ok := m["code"]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(v))); err == nil && n >= 400 {
			status = n
		}
	}
	if status == 0 {
		return nil
	}
	if !looksLikeQuotaText(msg) {
		return nil
	}
	return &sseError{status: status, msg: msg}
}

// looksLikeQueueFrame 报告文本里是否带排队特征（10605 / isQueued）。
// 只在扁平帧里用：嵌套帧的排队判定由 onChunk 里的 parseQueueFrame 负责。
func looksLikeQueueFrame(s string) bool {
	return strings.Contains(s, "10605") || strings.Contains(s, "isQueued")
}

// quotaWords 额度/余额耗尽的文案特征（小写匹配）。
//
// 为什么需要它：上游把「今日次数用完」以 402、403、甚至 200 包 code 三种形态发出，
// 状态码不足以判定；而这三种都被归成 ErrHardCredit（冷却整号到次日），错判的代价
// 是把健康号冻 12 小时，漏判的代价是用户每次都要等 180 秒才看到一句「断了」。
// 词表刻意保守：只收**明确指向额度/余额**的措辞，不收泛化的 "quota"。
var quotaWords = []string{
	"次数已达上限", "次数已用完", "额度不足", "额度用尽", "额度耗尽",
	"积分不足", "余额不足", "今日额度", "usage limit reached",
	"quota exhausted", "quota exceeded", "insufficient quota",
	"daily limit reached", "daily count exceeded", "credit not enough",
}

// looksLikeQuotaText 报告文本是否指向额度/余额耗尽。
func looksLikeQuotaText(s string) bool {
	if s == "" {
		return false
	}
	lower := strings.ToLower(s)
	for _, w := range quotaWords {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

// errTextOf 提取信封 body 里的错误文案（R90）。
//
// body 形如 {"code":"402","message":"<再嵌一层的 JSON 文本>"}，文案取 message 原文
// （内层 10605/RESOURCE_LIMIT 等结构化字段随之带出，不做二次解析）。
// 解不出时退回原始 body —— 宁可把上游原文交给客户端，也不要吞成空串。
func errTextOf(body string) string {
	var withMsg struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(body), &withMsg) == nil {
		if withMsg.Message != "" {
			return withMsg.Message
		}
		if withMsg.Code != "" {
			return "code=" + withMsg.Code
		}
	}
	if s := strings.TrimSpace(body); s != "" {
		return s
	}
	return "upstream business error"
}

// parseNestedSSE 逐行解析嵌套 SSE，每个有效 chunk 调 onChunk。
//   - body == "[DONE]" → 正常结束
//   - "event:finish" 行 → 忽略
//   - body 二次 unmarshal 失败 → 跳过该行（不致命）
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
			var env struct {
				Body            string `json:"body"`
				StatusCodeValue int    `json:"statusCodeValue"`
			}
			outerOK := json.Unmarshal([]byte(payload), &env) == nil
			innerOK := false
			// R90：信封 statusCodeValue >= 400 = 上游以 HTTP 200 包裹的业务失败
			// （402 额度耗尽 / 403 风控 / 400 参数错）。必须在 onChunk **之前**识别：
			// 这类帧没有 choices，当 chunk 透传等于给客户端一个空 delta，界面停在
			// 「正在等待模型…」直到网关掐断；而裸 error 又拿不到 kind ⇒ 不罚账号 ⇒
			// 粘性路由把下一次请求送回同一个号，反复撞同一堵墙。
			if outerOK && env.StatusCodeValue >= 400 {
				return &sseError{
					status: env.StatusCodeValue,
					msg:    errTextOf(env.Body),
				}
			}
			if outerOK && env.Body != "" {
				if env.Body == "[DONE]" {
					return nil // 上游显式收尾
				}
				var chunk map[string]any
				innerOK = json.Unmarshal([]byte(env.Body), &chunk) == nil
				if innerOK {
					if err := onChunk(chunk); err != nil {
						return err
					}
				}
			} else if outerOK {
				// 扁平帧兜底（R46）：没有 body 信封、整帧就是业务报文。
				// 排队/限流等错误帧在部分上游版本里以这种形态出现，若在此处丢弃，
				// 客户端既看不到 error 也会被补 [DONE] 伪装成正常收尾。
				//
				// R90 收窄了这里的行为：**只把「额度/账号级」扁平帧归一成 sseError**，
				// 其余一律沿用 R46 的「交给 onChunk 原样下传」契约。为什么不能全归一：
				//   - activity_test.go 的 TestFlatErrorFramePassesThrough 锁定
				//     `data: {"code":"110","message":"Billing daily count exceeded"}`
				//     必须**原样**到达客户端（含 code 字段），归一成 sseError 会把
				//     原文重写成 error 帧、丢掉 code，用户反而看不到上游原话；
				//   - queue_test.go 的 TestStreamQueueFrameBothEnvelopes 锁定扁平
				//     排队帧必须识别成 *QueueError（含 queueCount/waitTime），
				//     归一成 sseError 会把排队信息压成一句 message。
				// 这两条都是既有契约，不能被 R90 覆盖。
				var flat map[string]any
				if json.Unmarshal([]byte(payload), &flat) == nil && looksLikeErrorFrame(flat) {
					if se := accountLevelFrameError(flat); se != nil {
						return se
					}
					if err := onChunk(flat); err != nil {
						return err
					}
					innerOK = true
				}
			}
			if !outerOK {
				// R90 修：data: 行的**外层** JSON 就不合法 = 这一帧被切断在半路上。
				//
				// 此前只有「恰好卡在 EOF」时才判截断（见下面 err==io.EOF 分支），
				// 于是「上游写到一半断连，但残帧自带换行」（生产上极常见——残帧
				// 后面就跟着 \n\n 收尾）被判成干净收尾：客户端收到 [DONE]、网关
				// 记成一条成功流水，实际只收到半个帧。用户看到的「流已中断」里
				// 有一类就是它被掩盖成了「正常结束」。
				set()
			} else if err == io.EOF && !innerOK {
				set() // EOF 处这一行（内层）不是完整 JSON：半个帧
			}
		} else if err == io.EOF && strings.TrimSpace(line) != "" {
			// R90 修：EOF 落在一条**非 data: 且非空**的行上 = 上游在一个帧中间断掉。
			// （正常的空行收尾走到这里时 TrimSpace 为空，不判截断。）
			set()
		}
		if err == io.EOF {
			// 末尾没有换行收尾时，ReadString 会把「最后一个非空帧 + 其中的空行」一起
			// 交回来（如 "data: {...}\n"），故按最后一个**非空行**判定：
			// 它是空行 → 干净收尾；否则帧不完整（半帧）。
			if last := lastNonBlankLine(line); last != "" {
				set()
			}
			return nil
		}
	}
}

// aggregate 聚合完整 SSE 为单个 OpenAI chat.completion 响应。
// model 参数覆盖响应中的 model 字段（上游恒为 "auto"）。
// tool_calls 以流式 delta 到达（按 index 合并）。
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
			if delta, ok := c["delta"].(map[string]any); ok {
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

// sortInts 升序排序（避免引 sort 包只为三行）。
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
func streamAsOpenAI(w io.Writer, r io.Reader, model string, flush func(), onContent func()) (map[string]any, error) {
	var usage map[string]any
	truncated := false
	err := parseNestedSSE(r, func(chunk map[string]any) error {
		// 上游排队帧（10605 / isQueued）：**不得**当普通 chunk 透传——
		// 客户端既解析不出 choices（继续干等），也拿不到 error（无法重试）。
		// 改为发一帧 OpenAI 规范 error（带队列信息）并中止流，上层据此熔断。
		if q := parseQueueFrame(chunk); q != nil {
			payload, _ := json.Marshal(map[string]any{"error": map[string]any{
				"message": q.Error(), "type": "upstream_queued", "code": "model_queued",
			}})
			if _, werr := fmt.Fprintf(w, "data: %s\n\n", payload); werr != nil {
				return werr
			}
			if flush != nil {
				flush()
			}
			return q
		}
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
		if onContent != nil {
			onContent()
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
	// R90：流内业务错误（信封 statusCodeValue>=400 / 扁平错误帧）已归一成 *sseError。
	// 补一帧带原因与退避时长的 SSE error 并**不补 [DONE]**（R36）——补了就把失败
	// 伪装成正常结束，客户端会拿着空结果继续走，网关侧还把它记成一条成功流水。
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
// ContentSink 让上层在「上游第一个**真实内容**字节到达」时收到通知（可 nil）。
//
// R56b：受理帧一发出，TTFB 就不再代表「用户看到第一个字的时间」。把「首个内容」
// 单独通知出去，面板才能显示真实的「等首字」时长，而不是被连接建立时间掩盖。
type ContentSink func()

func StreamCapture(w http.ResponseWriter, r io.Reader, model string, onUsage func(map[string]any)) (map[string]any, error) {
	return StreamCaptureWithContent(w, r, model, onUsage, nil)
}

// StreamCaptureWithContent 同 StreamCapture，额外在首个内容帧下传时回调 once。
func StreamCaptureWithContent(w http.ResponseWriter, r io.Reader, model string, onUsage func(map[string]any), once ContentSink) (map[string]any, error) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	// R56b 一轮修正：受理帧由上层 acceptWriter 统一发（见 internal/server/handler.go），
	// 这里**不再重复发**。19:53 实测：两层都发会导致客户端收到两帧，
	// 而且 handler 侧先写会把响应头定成 text/plain，SSE 解析失败。
	fl, _ := w.(http.Flusher)
	flush := func() {
		if fl != nil {
			fl.Flush()
		}
	}
	// 活动探针：客户端还连着吗？已断开就没必要继续陪上游等（浪费连接与配额）。
	// 只记事实，不改流语义。
	watch := newActivityWatch(w, flush)
	defer watch.Close()
	// 首个**内容**帧通知（受理帧/注释帧不触发，见 live.MarkContent 的语义边界）。
	var contentOnce sync.Once
	var onContent func()
	if once != nil {
		onContent = func() { contentOnce.Do(once) }
	}
	usage, err := streamAsOpenAI(w, r, model, flush, onContent)
	if err == nil && onUsage != nil && usage != nil {
		onUsage(usage)
	}
	if err != nil {
		// R56b：**只在失败时**留一份系统级取证 —— 沉默期的观测点只在这类请求里
		// 才缺，正常请求不需要多写一个文件。
		capture.SaveSystem("stream-fail", map[string]any{
			"channel":   "qodercn",
			"model":     model,
			"error":     err.Error(),
			"watch":     watch.report(),
			"usage_got": usage != nil,
		})
	}
	return usage, err
}

// lastNonBlankLine 返回多行文本里最后一个非空行（去空白）。
//
// 用于 EOF 收尾判定：ReadString('\n') 在最后一段没有换行收尾时，会把「最后一个
// 完整帧 + 它自带的空行」一并交回（如 "data: {...}\n"），此时行内仍含换行，
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
