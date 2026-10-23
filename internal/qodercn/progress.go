// 等待期进度上报（R56）。
//
// 背景：上游排队时网关在内层保活重投（R55），这段时间**不向客户端发任何字节**
// —— 于是客户端界面只剩一个计时器，用户体感是「卡住了，只能挨个重启再试」。
// 这里把每一跳的排队/重投原因登记到面板（/api/live），让「在等」和「卡死」可区分。
//
// 并发模型：一个 Client 并发服务多个请求，故 sink 按 body 的 sha1 前缀隔离，
// 不能用单一全局回调（否则 A 请求的排队信息会挂到 B 请求的面板行上）。
package qodercn

import (
	"crypto/sha1"
	"encoding/hex"
	"omnigate/internal/provider"
)

// progKey 用 body 内容派生登记键（同一请求跨跳 body 不变 → 键稳定）。
func progKey(body []byte) string {
	sum := sha1.Sum(body)
	return hex.EncodeToString(sum[:8])
}

// ReportProgress 实现 provider.ProgressReporter：把等待期进度交给登记的回调。
// 任何情况下都不得影响请求本身（进度是旁路信息）。
func (c *Client) ReportProgress(body []byte, p provider.Progress) {
	if c == nil || len(body) == 0 {
		return
	}
	c.progMu.RLock()
	fn := c.progSink[progKey(body)]
	c.progMu.RUnlock()
	if fn == nil {
		return
	}
	// 回调由 handler 提供，只做内存登记，不会阻塞；recover 兜底防止第三方实现
	// 把 panic 带回请求主流程。
	defer func() { _ = recover() }()
	fn(p)
}

// registerProgress 登记/撤销某个请求的进度回调（nil = 撤销）。
func (c *Client) registerProgress(body []byte, fn func(provider.Progress)) {
	if c == nil || len(body) == 0 {
		return
	}
	k := progKey(body)
	c.progMu.Lock()
	defer c.progMu.Unlock()
	if fn == nil {
		delete(c.progSink, k)
		return
	}
	if c.progSink == nil {
		c.progSink = make(map[string]func(provider.Progress))
	}
	c.progSink[k] = fn
}

// SetProgressSink 便捷封装：给「一个具体请求」挂进度回调，返回注销函数。
// 之所以按 body 而不是按 Client 设置，见文件头并发模型说明。
func (c *Client) SetProgressSink(body []byte, fn func(provider.Progress)) (cancel func()) {
	c.registerProgress(body, fn)
	return func() { c.registerProgress(body, nil) }
}

// progressOf 是渠道内部用的便捷上报（body 已在作用域内）。
func (c *Client) progressOf(body []byte, phase string, pos, wait int, detail string) {
	c.ReportProgress(body, provider.Progress{Phase: phase, Pos: pos, Wait: wait, Detail: detail})
}
