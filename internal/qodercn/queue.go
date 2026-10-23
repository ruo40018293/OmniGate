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

	"omnigate/internal/provider"
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
// R55 改为 4 分钟（原 10 分钟）：上界必须小于保活预算 defaultQueueHoldBudget。
// 生产实测（2026-10-22 20:00）：10min 固定窗被保活循环每跳续期，剩余永远 ~578s >
// 预算，把「愿意等的请求」（DSH 轮次）永久锁在门外 10ms 硬拒 —— 熔断保护的是
// 不等的客户端，不能反过来惩罚等的。窗口同时按排队帧 waitTime 动态化（fuseWindow）。
//
// R57 提到 8 分钟：预算涨到 10min 后，4min 上界会让「等了 4 分钟才来的新请求」
// 在熔断已过期的情况下重复踩坑；8min 仍留 2min 让新请求进保活循环。
// queueBlockWindow 熔断窗上界。R57 定 8min（跟随当时 10min 预算），R58 预算回落到
// 5min 后同步收到 4min：**上界必须严格小于保活预算**，否则被保活循环每跳续期的
// 固定窗会把等待者永久锁在门外（R55c 的事故形态，TestFuseWindowBoundedByHoldBudget
// 守这条护栏）。4min 仍覆盖上游常态 waitTime（34~120s）三跳以上的实际窗口。
const queueBlockWindow = 4 * time.Minute

// queueHangWindow 首帧看门狗触发时的熔断窗口（短）：
// 上游挂起可能只是一次抖动（同一模型刚才是 0.3s 出首帧），罚太久会误伤；
// 90 秒足够让并发的后续请求快速失败，又不至于把可用模型锁死。
const queueHangWindow = 90 * time.Second

// fuseMinWindow 熔断窗下界。queueHangWindow 只描述「首帧看门狗」这类无 waitTime 的
// 场景；带 waitTime 的回执不该被这个下界抬到 90s（waitTime=36s 时抬到 90s 等于多封
// 54s，用户的新请求明明上游说 36s 后就能排上）。R57 拆出独立下界 30s。
const fuseMinWindow = 30 * time.Second

// fuseKey 归一化熔断键（R55-a，根因修复）。
//
// 历史 bug（2026-10-22 生产日志实锤）：Stream() 侧写熔断用的是 handler 传入的
// clientModel —— 客户端原始名，带渠道前缀与费率后缀（"qodercn/deepseek-flash (0.00x)"）；
// ChatStream() 侧检查用的是 rewriteModel 改写后的 body.model（"deepseek-flash"）。
// 两端键空间不同，生产熔断从未命中：17:21:30 记了 5630 的熔断，17 秒后的请求照样
// 打上游拿到 9176 新帧 —— 用户口中的「每次都重新开始排」。读写两侧统一过本函数：
// 去渠道前缀、去尾部括号后缀、转小写。
func fuseKey(model string) string {
	if i := strings.LastIndexByte(model, '/'); i >= 0 {
		model = model[i+1:]
	}
	if i := strings.Index(model, " ("); i >= 0 {
		model = model[:i]
	}
	return strings.ToLower(strings.TrimSpace(model))
}

// rememberQueue 记录排队熔断窗口（由 Client.Stream / ChatStream 保活循环在观察到排队后调用）。
// R55：窗口随排队帧 waitTime 动态化（见 fuseWindow），不再固定续满整窗。
func (c *Client) rememberQueue(model string, q *QueueError) {
	c.rememberQueueFor(model, q, fuseWindow(q))
}

// fuseWindow 熔断动态窗口：waitTime+30s，夹在 [fuseMinWindow(30s), queueBlockWindow(8min)]。
//
// 下界为什么不复用 queueHangWindow：那个 90s 是给「首帧看门狗触发、根本没有 waitTime」
// 的回执用的宽边界；带 waitTime 的回执有上游权威估计，不该被抬到 90s。
//
// R57 余量从 +60s 收到 +30s：熔断窗决定「多久之内新请求直接拿 429 而不进队列」，
// waitTime 已经是上游的估计，额外多给 60s 只会让新请求在队列松动时仍被挡在门外。
func fuseWindow(q *QueueError) time.Duration {
	if q != nil && q.WaitSeconds > 0 {
		d := time.Duration(q.WaitSeconds+30) * time.Second
		if d < fuseMinWindow {
			d = fuseMinWindow
		}
		if d > queueBlockWindow {
			d = queueBlockWindow
		}
		return d
	}
	return queueBlockWindow
}

// rememberQueueFor 指定窗口长度记录熔断（看门狗用短窗口，收到 10605 用长窗口）。
func (c *Client) rememberQueueFor(model string, q *QueueError, window time.Duration) {
	if c == nil || model == "" || q == nil {
		return
	}
	model = fuseKey(model)
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
	model = fuseKey(model)
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

// ── R55 排队保活重投 ──────────────────────────────────────────────────────────
//
// R46 的「快速失败」对会自动退避重试的客户端是友好的，但对把 429 当「本轮失败」
// 终止的客户端（DSH）就是「遇到排队就停、用户重发=重新排队」——上游排队不保留
// 位次，高峰期队列越排越深（实测 17:20 前后 679 → 9176），每次手动重发都要从头
// 排并陪上游等一段。官方客户端此时是「原地挂着等」，本质是替用户重投。
//
// 网关从 R55 起同样保活：观察到排队信号（三种形态：HTTP 429 快速拒绝、
// 流内首个数据帧为 10605、无字节挂起）时，只要还没花完保活预算，就按上游
// retryAfterSeconds 暂停后**在同一个客户端请求内**重投整个上游请求；预算耗尽
// 才回落 R46 行为（429 + 熔断），熔断同时保护其它并发请求不陪等。
//
// 前提（抓包与 parseNestedSSE 注释证实）：上游不会先吐内容帧再吐排队帧，
// 排队判定发生在任何内容之前，所以丢弃已读字节重投是安全的。
const (
	// defaultQueueHoldBudget 单请求最大保活时长 = 5 分钟（R58）。
	//
	// 历史：R55 定 5min → R57 提到 10min → R57 同晚改成 0（不限）→ 当晚回落到 5min。
	// 用户 2026-10-23 20:24 曾说「不断他一直排到好就行」，但 20:29-20:44 实测证明
	// 那条路走不通：qodercn 免费池高峰时队列持续增长，12 分钟里名次在
	// 3532→1350→4042→2935→4200 之间拉锯、一次都没降到 1200 以下，重投 14 次也
	// 排不上。无限等只是把「卡住」换成「看起来在动但一直不动」，还白占一条上游
	// 连接和一份并发。用户最终选择「弄个上限时间吧 5 分钟」。
	//
	// 5min 的依据：上游给的 waitTime 常态 34~120s，5 分钟够走 3~6 跳；超过 5 分钟
	// 还在排队说明处在队列增长期，这时候继续等的边际收益是负的——不如把 429 和
	// 准确 retry_after 交回客户端，让用户自己决定换模型还是等。
	//
	// 0 = 不设上限仍被 holdRemaining 支持（测试与临时排障用），但默认不启用。
	defaultQueueHoldBudget = 5 * time.Minute
	// defaultQueueHoldPause 排队帧没给 waitTime 也没有 retryAfterSeconds 时的重投间隔。
	// 注意：有 waitTime 时不走这里，走 holdPauseFor 按上游时间等（R57）。
	defaultQueueHoldPause = 30 * time.Second
	// queueScanStall 首帧扫描的帧间静默阈值：排队时上游先发保活字节、~180s 后
	// 才吐 10605；保活字节可能迟迟不成「行」，超过本阈值无新完整行即按排队挂起
	// 处理（重投/熔断），比旧行为（idle.Monitor 60s 后掐成截断错误）更友好。
	queueScanStall = 75 * time.Second
)

// holdBudget 返回保活预算（0 值兜底默认；测试注入小值）。
func (c *Client) holdBudget() time.Duration {
	if c != nil && c.queueHoldBudget > 0 {
		return c.queueHoldBudget
	}
	return defaultQueueHoldBudget
}

// holdBudgetText 把预算渲染成日志文案（0 表示不设上限，仅测试/排障路径）。
func (c *Client) holdBudgetText() string {
	if b := c.holdBudget(); b > 0 {
		return b.Round(time.Second).String()
	}
	return "不限（直到排上或客户端断开）"
}

// holdPause 重投间隔：注入值优先（测试）；否则用上游 retryAfterSeconds
// 夹在 [5s, 60s]（上游期望客户端就按这个节奏轮询）；再否则默认 30s。
func (c *Client) holdPause(q *QueueError) time.Duration {
	if c != nil && c.queueHoldPause > 0 {
		return c.queueHoldPause
	}
	if q != nil && q.RetryAfter > 0 {
		d := time.Duration(q.RetryAfter) * time.Second
		if d < 5*time.Second {
			d = 5 * time.Second
		}
		if d > time.Minute {
			d = time.Minute
		}
		return d
	}
	return defaultQueueHoldPause
}

// holdPauseFor R57 等待时长：按上游 tells 的 waitTime 决定，而不是固定 30s。
//
// 生产实测（2026-10-23 20:05-20:08，同一个请求五跳）：
//
//	20:05:58 前方 4339 等待 119s
//	20:06:28 前方 2138 等待  59s   ← 名次在降
//	20:06:58 前方 4390 等待 120s  ← 又回队尾
//	20:07:29 前方 2348 等待  65s
//	20:07:59 前方 4730 等待 129s
//
// 每次重投都是**新发一个上游请求**，上一个请求的排队位次随之作废（上游按请求
// 排，不按账号留位）。所以固定间隔重投 = 每 30s 放弃一次已排到的位置，名次永远
// 追不回来，用户看到的就是「快到了又从头排」。
//
// 现在的规则：等上游说的那个时间（waitTime），并在快到点时提前一点重投，
// 避免上游自己 180s 掐断把位次一起带走。上限仍由 holdBudget 兜底。
func (c *Client) holdPauseFor(q *QueueError) time.Duration {
	if c != nil && c.queueHoldPause > 0 {
		return c.queueHoldPause // 测试注入
	}
	if q == nil || q.WaitSeconds <= 0 {
		return c.holdPause(q) // 没有 waitTime 的回执（含首帧挂起/静默）退回 retryAfter 节奏
	}
	pause := time.Duration(q.WaitSeconds) * time.Second
	// 提前 8s（至少留 3s）重投：上游约 180s 会自己断，等到最后一刻才发现断流，
	// 位次已经丢了；提前一点让我们主动发起下一个请求。
	lead := pause / 20
	if lead < 3*time.Second {
		lead = 3 * time.Second
	}
	if lead > 8*time.Second {
		lead = 8 * time.Second
	}
	pause -= lead
	if pause < 2*time.Second {
		pause = 2 * time.Second
	}
	if pause > 5*time.Minute {
		pause = 5 * time.Minute
	}
	return pause
}

// holdRemaining 判断从 start 起再暂停 pause 是否仍在预算内（R57 用 holdPauseFor）。
// 预算为 0 表示不设上限（仅测试/排障），永远返回 ok。
func (c *Client) holdRemaining(start time.Time, q *QueueError) (time.Duration, bool) {
	pause := c.holdPauseFor(q)
	budget := c.holdBudget()
	if budget <= 0 {
		return pause, true // 预算为 0：不设上限
	}
	if time.Since(start)+pause > budget {
		return 0, false
	}
	return pause, true
}

// classifyLeadFrame 给 200 流的「首个数据帧」定性（R55 形态②识别）。
// 行处理与 parseNestedSSE 对齐：data: 前缀、body 信封（内层 [DONE] 视为内容）、
// 扁平业务帧。返回 q!=nil 表示排队帧；content=true 表示已见内容/收尾帧，
// 保活扫描应停止并把字节原样交还调用方。
func classifyLeadFrame(line string) (q *QueueError, content bool) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data:") {
		return nil, false
	}
	payload := strings.TrimPrefix(line, "data:")
	var env struct {
		Body string `json:"body"`
	}
	if json.Unmarshal([]byte(payload), &env) == nil && env.Body != "" {
		if env.Body == "[DONE]" {
			return nil, true
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(env.Body), &chunk) == nil {
			if qq := parseQueueFrame(chunk); qq != nil {
				return qq, false
			}
			return nil, true // 有 body 信封且非排队：内容帧（含无法识别的），放行
		}
		return nil, true // 内层不是 JSON：交给下游按旧路径处理
	}
	var flat map[string]any
	if json.Unmarshal([]byte(payload), &flat) == nil {
		if looksLikeErrorFrame(flat) {
			if qq := parseQueueFrame(flat); qq != nil {
				return qq, false
			}
			return nil, true // 其它业务错误帧：原文放行（与旧行为一致）
		}
		if _, ok := flat["choices"]; ok {
			return nil, true
		}
	}
	return nil, false // 保活/注释/无法解析的碎片：继续攒
}
