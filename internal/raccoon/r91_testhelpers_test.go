package raccoon

import "time"

// resetSessions 清空会话表与序号，供测试隔离（生产代码不调用）。
func resetSessions() {
	sessMu.Lock()
	defer sessMu.Unlock()
	sessByKey = map[string]sessionEntry{}
	sessSeq = 0
}

// forceExpire 把某个 key 的到期时间拨到过去，模拟过了 24h 轮换期。
func forceExpire(key string) {
	sessMu.Lock()
	defer sessMu.Unlock()
	if e, ok := sessByKey[key]; ok {
		e.expires = time.Now().Add(-time.Minute)
		sessByKey[key] = e
	}
}
