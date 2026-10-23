// 等待期活动探针（R56b）。
//
// 事故（2026-10-23）：qodercn 上游只发保活碎片、约 180s 后零内容断开。那 180 秒里
// 网关不向客户端写任何字节，app.log 只有结束之后的一行 —— 「沉默期」完全没有观测点，
// 于是「上游挂起」「网关卡住」「客户端已断开」三种完全不同的故障长得一模一样。
//
// 本探针只回答一个问题：**沉默期内，客户端连接是否仍然有效？**
// 用途有三：
//  1. 客户端已断开 → 尽快放弃本次上游等待（省连接、省配额）；
//  2. 仍然有效 → 保持等待（用户的 DSH 会话就在等这次回答）；
//  3. 两者都记进系统级取证，事后能区分「上游挂了」与「客户端跑了」。
//
// 语义约定：只做观测，不写客户端的流（不插入任何 data 帧）。
package qodercn

import (
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// activityWatch 周期性探测客户端连接是否仍然可写。
//
// 探测方式：向 ResponseWriter 写一帧 SSE **注释**（": ping"）并 flush。
// 注释帧按 SSE 规范必须被客户端忽略，不会污染 OpenAI 数据流；而对 HTTP 层
// 来说这是一次真实写入 —— 写失败即说明客户端已断开（broken pipe / 连接重置）。
type activityWatch struct {
	lastWrite atomic.Int64 // unix nano：最近一次成功写入
	writeErr  atomic.Int64 // 1 = 检测到写失败（客户端已断）
	pings     atomic.Int64 // 探测帧计数（取证用）
	w         io.Writer
	flush     func()
	stop      chan struct{}
}

// newActivityWatch 启动探针；interval<=0 用默认 20s。
//
// 20s 的取值依据：上游保活碎片间隔是秒级、零内容断开发生在上百秒量级，20s 足以
// 在沉默期留下多个观测点，又不会让客户端看到过密的注释帧。
func newActivityWatch(w io.Writer, flush func()) *activityWatch {
	if flush == nil {
		flush = func() {}
	}
	a := &activityWatch{w: w, flush: flush, stop: make(chan struct{})}
	a.touch()
	go a.loop(20 * time.Second)
	return a
}

func (a *activityWatch) touch() { a.lastWrite.Store(time.Now().UnixNano()) }

func (a *activityWatch) loop(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-a.stop:
			return
		case <-t.C:
			if a.dead() {
				return // 已断开：不再白试，省 CPU 与日志
			}
			// 注释帧：SSE 规范要求客户端忽略非 data 行。
			if _, err := io.WriteString(a.w, ": ping\n\n"); err != nil {
				a.writeErr.Store(1)
				return
			}
			a.flush()
			a.pings.Add(1)
			a.touch()
		}
	}
}

// dead 客户端是否已断开（写失败过）。
func (a *activityWatch) dead() bool { return a.writeErr.Load() == 1 }

// Close 停止探针（幂等）。
func (a *activityWatch) Close() {
	select {
	case <-a.stop:
	default:
		close(a.stop)
	}
}

// SilentFor 距离最近一次成功写入过了多久（沉默期长度，取证与判定用）。
func (a *activityWatch) SilentFor() time.Duration {
	ns := a.lastWrite.Load()
	if ns == 0 {
		return 0
	}
	return time.Since(time.Unix(0, ns))
}

// report 生成取证字段（落进 capture.SaveSystem）。
func (a *activityWatch) report() map[string]any {
	if a == nil {
		return nil
	}
	return map[string]any{
		"client_dead":     a.dead(),
		"silent_for_ms":   a.SilentFor().Milliseconds(),
		"probe_pings":     a.pings.Load(),
		"probe_interval_s": 20,
	}
}

// 编译期断言：ResponseWriter 的 Flusher 分支由 StreamCapture 负责，
// 这里只依赖 io.Writer，保证测试可以传 bytes.Buffer。
var _ io.Writer = (http.ResponseWriter)(nil)
