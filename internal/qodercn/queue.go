// queue.go 上游「模型排队」（code 10605 / isQueued）识别与短时熔断。
//
// 背景（2026-10-20 实盘）：qodercn/deepseek-flash（上游 modelKey=dfmodel）进入排队制。
// 上游会先把 HTTP 响应挂住约 180 秒，然后才在 SSE 流里吐出一帧：
//
//	{"code":"403","message":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":true,
//	 \\\"modelKey\\\":\\\"dfmodel\\\",\\\"queueCount\\\":3058,\\\"queueType\\\":\\\"p3\\\",
//	 \\\"retryAfterSeconds\\\":30,\\\"serviceAvailable\\\":true,\\\"waitTime\\\":185}\"}"}
//
// 旧实现把这帧当普通 chunk 透传给客户端：客户端既解析不出 choices（于是继续等），
// 也拿不到 error（于是无法重试/换模型），表现就是用户口中的「点了要排队、卡住」。
// 同一时刻同账号的 qodercn/qwen3.8-flash 1.26s 返回 200 —— 故这是**模型级**排队，
// 与账号、请求头都无关（对照实验，见 AGENTS.md R46）。
package qodercn

import (
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"workbuddy-api/internal/provider"
)

// QueueError 上游排队错误。实现 provider.StreamErrorClassifier：
// 排队是模型侧容量问题，**不是账号故障**，故 Kind 恒为 ErrPassthrough
// （handler 的 default 分支只记日志、不冷却账号）。
type QueueError struct {
	ModelKey    string
	QueueCount  int
	WaitSeconds int
	RetryAfter  int
	// Hang 为 true 表示「排队帧尚未到达，但首帧看门狗已超时」——
	// 上游把 HTTP body 挂住（实测 180s 才吐 10605），网关先按排队处理。
	Hang bool
}

// Error 人类可读，带足以让用户决策的信息（等多久 / 还有多少人在前面）。
func (e *QueueError) Error() string {
	if e == nil {
		return ""
	}
	parts := []string{"上游模型排队中"}
	if e.Hang {
		parts = []string{"上游排队：连接已建立但迟迟不出首帧（看门狗超时）"}
	}
	if e.ModelKey != "" {
		parts = append(parts, "model="+e.ModelKey)
	}
	if e.QueueCount > 0 {
		parts = append(parts, fmt.Sprintf("前方=%d", e.QueueCount))
	}
	if e.WaitSeconds > 0 {
		parts = append(parts, fmt.Sprintf("预计等待=%ds", e.WaitSeconds))
	}
	if e.RetryAfter > 0 {
		parts = append(parts, fmt.Sprintf("retry_after=%ds", e.RetryAfter))
	}
	return strings.Join(parts, " ")
}

// Kind 实现 provider.StreamErrorClassifier：请求级/模型级，不罚账号。
func (e *QueueError) Kind() provider.ErrKind { return provider.ErrPassthrough }

// queueFieldRes 从（已反转义的）报文里抽字段。上游把 JSON 嵌成多层字符串，
// 反转义后形如 "queueCount":3058 —— 故按裸键值匹配即可。
var (
	reQueueCount   = regexp.MustCompile(`"queueCount"\s*:\s*(\d+)`)
	reQueueWait    = regexp.MustCompile(`"waitTime"\s*:\s*(\d+)`)
	reQueueRetry   = regexp.MustCompile(`"retryAfterSeconds"\s*:\s*(\d+)`)
	reQueueModel   = regexp.MustCompile(`"modelKey"\s*:\s*"([^"]*)"`)
	reIsQueuedTrue = regexp.MustCompile(`(?i)"isQueued"\s*:\s*true`)
)

// parseQueueBody 解析 HTTP 级错误响应体里的排队帧（R46 第三条信号路径）。
//
// 2026-10-21 18:04 生产补充：上游饱和时除「挂 180s 后在流里吐帧」外，还会
// **直接快速回 HTTP 429 + 排队 body**（实测 dur≈700ms）。该路径此前只透传
// 不熔断，客户端每次重试都要重新打上游；识别后同样写熔断窗口。
// body 兼容扁平 JSON 与带 body 信封的嵌套 JSON 两种形态。
func parseQueueBody(raw []byte) *QueueError {
	if len(raw) == 0 {
		return nil
	}
	s := string(raw)
	if !strings.Contains(s, "10605") && !strings.Contains(s, "isQueued") {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	if q := parseQueueFrame(m); q != nil {
		return q
	}
	// body 信封形态：{"headers":...,"body":"<排队帧 JSON 字符串>"}
	if inner, ok := m["body"].(string); ok {
		var im map[string]any
		if json.Unmarshal([]byte(inner), &im) == nil {
			return parseQueueFrame(im)
		}
	}
	return nil
}

// parseQueueFrame 从上游 chunk 里识别排队帧；非排队帧返回 nil（零成本快路径）。
//
// 判据取「10605」或「isQueued」任一：实测两者同时出现，但不同版本的
// 包装层可能只保留其中一个（与 R31 的 10001 同类：业务码优先于文案）。
func parseQueueFrame(chunk map[string]any) *QueueError {
	if chunk == nil {
		return nil
	}
	// 快路径：只有带 code 的帧才可能是业务错误（正常 chunk 走 choices）。
	code, _ := chunk["code"].(string)
	if code == "" {
		if c, ok := chunk["code"]; ok {
			code = fmt.Sprint(c)
		}
	}
	if code == "" && chunk["message"] == nil {
		return nil
	}
	raw := fmt.Sprint(chunk)
	// 多层 JSON 字符串 → 反转义后按裸键值抽字段/判定。
	flat := strings.ReplaceAll(raw, `\"`, `"`)
	flat = strings.ReplaceAll(flat, `\\`, `\`)
	// 判据：业务码 10605，或明确的 isQueued:true。
	// 注意必须判 true —— 实测存在 isQueued:false 的「未排队」回执（code 10606），
	// 只认子串 "isQueued" 会把它误判成排队（回归测试 TestParseQueueFrameNegative）。
	if !strings.Contains(flat, "10605") && !reIsQueuedTrue.MatchString(flat) {
		return nil
	}
	q := &QueueError{}
	if m := reQueueModel.FindStringSubmatch(flat); len(m) == 2 {
		q.ModelKey = m[1]
	}
	q.QueueCount = firstInt(reQueueCount, flat)
	q.WaitSeconds = firstInt(reQueueWait, flat)
	q.RetryAfter = firstInt(reQueueRetry, flat)
	return q
}

// firstInt 取正则首个捕获组的整数；不匹配返回 0。
func firstInt(re *regexp.Regexp, s string) int {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// queueBlockWindow 排队熔断窗口：一旦观察到排队，这段时间内对该模型直接
// 快速失败，避免每个请求都陪上游挂 180 秒。
//
// 取值依据：实测 queueCount=3058、waitTime=185s，且队列是全局共享的免费池，
// 深度在分钟级不会迅速清空；10 分钟窗口把「每请求 180s」压缩成「每次探测后
// 10 分钟内毫秒级失败」，客户端可据此立刻换模型。
const queueBlockWindow = 10 * time.Minute

// queueHangWindow 首帧看门狗触发时的熔断窗口（短）：
// 上游挂起可能只是一次抖动（同一模型刚才是 0.3s 出首帧），罚太久会误伤；
// 90 秒足够让并发的后续请求快速失败，又不至于把可用模型锁死。
const queueHangWindow = 90 * time.Second

// rememberQueue 记录排队熔断窗口（由 Client.Stream 在观察到排队后调用）。
func (c *Client) rememberQueue(model string, q *QueueError) {
	c.rememberQueueFor(model, q, queueBlockWindow)
}

// rememberQueueFor 指定窗口长度记录熔断（看门狗用短窗口，收到 10605 用长窗口）。
func (c *Client) rememberQueueFor(model string, q *QueueError, window time.Duration) {
	if c == nil || model == "" || q == nil {
		return
	}
	c.queueMu.Lock()
	if c.queueUntil == nil {
		c.queueUntil = map[string]time.Time{}
		c.queueInfo = map[string]*QueueError{}
	}
	c.queueUntil[model] = time.Now().Add(window)
	c.queueInfo[model] = q
	c.queueMu.Unlock()
	log.Printf("qodercn model queued model=%s；%v 内对该模型快速失败（%s）",
		model, window, q.Error())
}

// queueBlocked 窗口内返回（排队信息, 剩余时间），否则 (nil, 0)。
// 剩余时间用于给客户端动态 retry_after：越早问越短，退避才准。
func (c *Client) queueBlocked(model string) (*QueueError, time.Duration) {
	if c == nil || model == "" {
		return nil, 0
	}
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	until, ok := c.queueUntil[model]
	if !ok {
		return nil, 0
	}
	remain := time.Until(until)
	if remain <= 0 {
		return nil, 0
	}
	return c.queueInfo[model], remain
}

// queuedBody 生成 OpenAI 兼容错误体：handler 按 ErrPassthrough 原文透传，
// 客户端看到合法 429 + 可读原因，可立即换模型或按 retry_after 退避。
//
// retryAfter 由调用方给（熔断期内用剩余秒数，首次触发用窗口长度），
// 这样客户端拿到的退避时长是「现在还要等多久」而不是一个固定值。
func queuedBody(model string, q *QueueError, retryAfter int) []byte {
	msg := fmt.Sprintf("模型 %s 上游排队中（前方 %d 个请求，预计等待 %d 秒）：请稍后重试或改用其它模型",
		model, q.QueueCount, q.WaitSeconds)
	if q.QueueCount == 0 && q.WaitSeconds == 0 {
		msg = fmt.Sprintf("模型 %s 上游排队中：请稍后重试或改用其它模型", model)
	}
	if q.Hang {
		msg = fmt.Sprintf("模型 %s 上游排队：连接已建立但没有返回任何数据（官方客户端此时同样显示排队中）"+
			"；网关已对该模型快速失败 %d 秒，请稍后重试或改用其它模型", model, retryAfter)
	}
	out, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": msg, "type": "upstream_queued", "code": "model_queued",
		"retry_after": retryAfter, "queue_count": q.QueueCount, "wait_seconds": q.WaitSeconds,
		"upstream_key": q.ModelKey,
	}})
	return out
}

// defaultHangSeconds 供文案使用：看门狗阈值秒数（未配置时 30）。
func (c *Client) defaultHangSeconds() int {
	if c != nil && c.FirstByteTimeout > 0 {
		return int(c.FirstByteTimeout / time.Second)
	}
	return int(defaultFirstByteTimeout / time.Second)
}
