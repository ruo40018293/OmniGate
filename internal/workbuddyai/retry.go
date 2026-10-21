// retry.go 上游请求重试策略。
//
// 实测背景：www.workbuddy.ai 的 openresty/APISIX 网关存在间歇性抖动，
// 同一请求连发多次会随机出现 502/503/504（直连亦如此，非本层引入）。
// 504 恒定卡在 ~12.3s（网关自身超时阈值）。
//
// 策略：
//   - 仅对网关类 5xx（502/503/504）与传输层错误重试；
//   - 4xx 一律不重试：402/403 疑似风控或余额不足，400/429 是业务/限流语义，
//     重试只会放大问题（429 频繁重试还可能触发风控）；
//   - 5xx 中的 500 视为上游服务端错误，同样重试。
package workbuddyai

import (
	"errors"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"time"
)

// 重试参数：总尝试次数与退避基准（指数退避 + 抖动）。
const (
	retryMaxAttempts = 3
	retryBaseDelay   = 400 * time.Millisecond
	// busyBaseDelay 「模型繁忙」的重试基准：比 5xx 网关抖动略长，
	// 因为上游是按共享免费池容量放行，间隔太密只是再撞一次。
	busyBaseDelay = 500 * time.Millisecond
)

// modelBusyMarkers 共享免费池饱和的响应标记（HTTP 429 + code=14003 / "Model busy"）。
var modelBusyMarkers = []string{`"code":14003`, "model busy", "模型繁忙"}

// modelBusy 判断响应是否为「模型繁忙」型限流。
//
// 实测（2026-10-20 15:00–15:02，账号 e0f0b910 + deepseek-v4.1-flash）：
//   - 同一请求分别用官方 5.8.4 全套头与网关修复后的头，两者都既可能 200 也可能 429；
//   - 固定 8 秒间隔连发 5 次，结果为 429/200/429/200/200。
//
// 即 14003 与「请求头是否像官方客户端」无关，是免费模型池的瞬时容量波动。
// 官方 Electron 客户端遇到它同样是立刻重试，故本层也重试；但**不得**据此冷却
// 模型 60s——否则上游的瞬时抖动会被放大成整分钟的 no_healthy_account
// （2026-10-20 14:50–15:00 实测：14:50/14:52/14:58 各一次 429 把 deepseek-v4.1-flash
// 连续冻住，而同一时刻官方客户端可用）。
func modelBusy(status int, body []byte) bool {
	if status != http.StatusTooManyRequests {
		return false
	}
	s := strings.ToLower(string(body))
	for _, m := range modelBusyMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// retryableStatus 判断 HTTP 状态码是否值得重试。
// 仅 5xx 网关/服务端错误可重试；4xx 明确不重试。
func retryableStatus(status int) bool {
	return status >= 500 && status <= 599
}

// retryableErr 判断传输层错误是否值得重试。
// 连接被重置/中断/eof 等瞬态错误重试；证书类/参数类错误不重试。
func retryableErr(err error) bool {
	if err == nil {
		return false
	}
	// 明确不可重试：证书校验、协议不支持等
	var certErr *net.OpError // 仍可能是瞬态，继续判断
	_ = certErr
	s := strings.ToLower(err.Error())
	for _, noRetry := range []string{"x509", "certificate", "unsupported protocol", "malformed"} {
		if strings.Contains(s, noRetry) {
			return false
		}
	}
	// 超时、连接重置、EOF、对端关闭等 → 可重试
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	for _, yes := range []string{"connection reset", "connection refused", "broken pipe",
		"eof", "timeout", "no such host", "connection attempt failed", "empty reply"} {
		if strings.Contains(s, yes) {
			return true
		}
	}
	return false
}

// backoff 第 attempt 次重试前的等待时长（指数退避 + 抖动，避免请求对齐）。
func backoff(attempt int) time.Duration {
	d := retryBaseDelay << (attempt - 1)
	// 抖动 ±40%
	jitter := time.Duration(rand.Int63n(int64(d/2+1))) - d/4
	return d + jitter
}

// busyBackoff 「模型繁忙」第 attempt 次重试前的等待时长（指数退避 + 抖动）。
func busyBackoff(attempt int) time.Duration {
	d := busyBaseDelay << (attempt - 1)
	// 抖动 ±40%
	jitter := time.Duration(rand.Int63n(int64(d/2+1))) - d/4
	return d + jitter
}
