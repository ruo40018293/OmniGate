package server

import (
	"bytes"
	"io"
	"net/http"
	"sync"
)

// contentWatcher 包在流式响应外层，识别「第一帧真正的内容」。
//
// 为什么需要它（R93）：Qoder 系渠道当日额度耗尽时，上游会先发一帧 role、
// 随后完全沉默，直到约 180 秒才自行断开（R90 实测 qodercn 47 次 / qodercom 30 次，
// p50=180.1s，而写给客户端的字节只有 81~513）。这期间我们既不报错也不换号，
// 客户端界面上只有一个计时器 —— 用户只会觉得卡死了。
//
// 上游项目对照：sub2api 的 client_first_message_timeout_seconds 与 grok2api 的
// requestRetry.holdTimeout 是同一个思路 —— 给「首条内容」设截止，超了就认输，
// 不要陪上游等满它自己的 180 秒。
//
// 选在 handler 层按出站 SSE 帧判断，而不是改 11 个渠道：各渠道 Stream/StreamContent
// 写出的都是规范化后的 OpenAI chunk，内容必然落在 choices[].delta 的
// content / reasoning_content / tool_calls 三者之一。
type contentWatcher struct {
	http.ResponseWriter
	flusher http.Flusher
	once    sync.Once
	seen    chan struct{}
}

// contentKeys 是「内容字段」的字节前缀。判断规则：键后面紧跟的字符不是 `"`，
// 说明是非空字符串（`"content":""` 是 role 首帧的形态，不算内容）。
var contentKeys = [][]byte{
	[]byte(`"content":"`),
	[]byte(`"reasoning_content":"`),
}

// toolCallsField 单独判：值形如 `[{"id":...`，`[]` / `null` 都不算内容。
// 冒号与括号之间允许空白（不同渠道的 Marshal 习惯不同）。
var toolCallsField = []byte(`"tool_calls"`)

func newContentWatcher(w http.ResponseWriter) *contentWatcher {
	cw := &contentWatcher{ResponseWriter: w, seen: make(chan struct{})}
	if f, ok := w.(http.Flusher); ok {
		cw.flusher = f
	}
	return cw
}

func (c *contentWatcher) Write(p []byte) (int, error) {
	if !c.HasContent() && hasContentFrame(p) {
		c.once.Do(func() { close(c.seen) })
	}
	return c.ResponseWriter.Write(p)
}

// Flush 必须转发：各渠道都用 `w.(http.Flusher)` 逐帧推流，
// 漏掉这个方法会把 SSE 退化成「攒完一次性返回」。
func (c *contentWatcher) Flush() {
	if c.flusher != nil {
		c.flusher.Flush()
	}
}

// Seen 在首帧内容出现时关闭，供看门狗 select。
func (c *contentWatcher) Seen() <-chan struct{} { return c.seen }

// HasContent 非阻塞查询。
func (c *contentWatcher) HasContent() bool {
	select {
	case <-c.seen:
		return true
	default:
		return false
	}
}

func isJSONSpace(b byte) bool {
	return b == ' ' || b == '\n' || b == '\r' || b == '\t'
}

// hasContentFrame 判断这一批出站字节里是否含「非空内容」。
// 逐键扫描而不是整块 JSON 解析：一批 Write 里可能有多帧，且分帧边界不保证落在
// JSON 边界上（渠道各自 Flush 的粒度不同）。
func hasContentFrame(p []byte) bool {
	for _, key := range contentKeys {
		idx := 0
		for {
			i := bytes.Index(p[idx:], key)
			if i < 0 {
				break
			}
			j := idx + i + len(key)
			if j < len(p) && p[j] != '"' {
				return true
			}
			idx = j
		}
	}
	idx := 0
	for {
		i := bytes.Index(p[idx:], toolCallsField)
		if i < 0 {
			return false
		}
		j := idx + i + len(toolCallsField)
		for j < len(p) && isJSONSpace(p[j]) {
			j++
		}
		if j < len(p) && p[j] == ':' {
			j++
		}
		for j < len(p) && isJSONSpace(p[j]) {
			j++
		}
		if j < len(p) && p[j] == '[' {
			k := j + 1
			for k < len(p) && isJSONSpace(p[k]) {
				k++
			}
			// `[]`（空数组）与 `null` 都不算内容。
			if k < len(p) && p[k] == '{' {
				return true
			}
		}
		idx = j
	}
}

// firstContentReader 让「首内容截止」有一个**确定的原因**。
//
// 直接关底流也能解除阻塞的 Read，但读取方拿到的是底层错误串
// （pipe 关掉是 io.ErrClosedPipe、连接关掉是 use of closed network connection），
// 于是渠道按 R90 的分类只会把它记成 upstream_truncated —— 首内容超时与上游真断
// 又变成同形了。这里改成：先毒化（此后每次 Read 立即返回 ErrFirstContentTimeout），
// 再关底流解除阻塞。
type firstContentReader struct {
	rc io.ReadCloser
	mu sync.Mutex
	// poisoned 非 nil 表示看门狗已开枪；此后所有 Read 都返回它。
	poisoned error
}

func newFirstContentReader(rc io.ReadCloser) *firstContentReader {
	return &firstContentReader{rc: rc}
}

func (r *firstContentReader) Read(p []byte) (int, error) {
	if err := r.poisonErr(); err != nil {
		return 0, err
	}
	n, err := r.rc.Read(p)
	// 读返回后再查一次：毒化可能发生在阻塞期间（毒化先置位再关底流，
	// 所以这里一定看得到）。
	if perr := r.poisonErr(); perr != nil {
		return n, perr
	}
	return n, err
}

func (r *firstContentReader) Close() error { return r.rc.Close() }

func (r *firstContentReader) poisonErr() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.poisoned
}

// poison 由看门狗调用：让后续读取立刻以 err 结束，并关底流解除当前阻塞。
func (r *firstContentReader) poison(err error) {
	r.mu.Lock()
	if r.poisoned == nil {
		r.poisoned = err
	}
	r.mu.Unlock()
	_ = r.rc.Close()
}
