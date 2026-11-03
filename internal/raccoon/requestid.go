package raccoon

// R91 的请求侧配套：官方 box-agent 在每次 LLM 请求上带一串「客户端身份 + 关联」头，
// 其中三个值必须在请求时才确定（系统版本、会话 ID、轮次 ID）。本文件集中给出这三个，
// 只由 headers.go 的 ApplyHeaders 消费，不另设配置项（无消费者的抽象不加）。

import (
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"omnigate/internal/auth"
)

// osVersionOnce 缓存 gatewayOSVersion：系统版本在一次进程生命期内不变，
// 逐请求读盘/调系统 API 毫无收益（本渠道请求间隔常为分钟级）。
var (
	osVersionOnce sync.Once
	osVersion     string
)

func gatewayOSVersion() string {
	osVersionOnce.Do(func() {
		osVersion = readOSVersion()
	})
	return osVersion
}

// sessionRotation 是 X-RACCOON-Session-ID 的轮换周期。
//
// 官方实证（2026-10-31 三轮实验，见 R148）：一个 ACP 会话在一个项目生命周期内**固定不变**，
// 实测跨 37 分钟、150+ 次 LLM 调用、遇 429 也不换；只有用户**新建项目**才发 `session/new`
// 且序号 +1，切回旧项目直接复用旧会话。网关没有「项目」概念，故用「按账号+模型固定 + 24h 轮换」
// 近似：24h 远大于任何单次对话时长，绝不会把一次对话拆成多个会话（那正是要修的旧行为），
// 同时避免一个会话在进程存活期内无限增长。
const sessionRotation = 24 * time.Hour

// sessionEntry 是一个 key 对应的会话 ID 及其到期时刻。
type sessionEntry struct {
	id      string
	expires time.Time
}

var (
	sessMu    sync.Mutex
	sessByKey = map[string]sessionEntry{}
	// sessSeq 是进程内会话序号，对齐官方 `sess-<N>-<hex>` 的 N（重启归零，
	// 实测官方同进程内新建则递增到 sess-1/sess-2）。
	sessSeq uint64
)

// sessionKeyOf 组装会话分桶键：**账号 + 模型**。
//
// 为什么必须带账号：会话 ID 会随请求发往上游，若两个不同账号共用一个 `sess-`，
// 上游看到的是「同一个会话出现在两个账号的请求里」——官方客户端不可能这样
// （它一个会话自始至终属于登录的那个账号）。raccoon 只有 1 个账号时这条不起作用，
// 但只要池里出现第 2 个账号、或粘性路由到阈值换号，旧 key（只有模型名）就会让
// 同一个 `sess-` 从账号 A 漂到账号 B。
func sessionKeyOf(a *auth.Auth, model string) string {
	return accountKey(a) + "|" + model
}

// accountKey 取账号维度标识：优先 UID，退回昵称，再退回 token 指纹，最后才用固定串。
//
// 不能只依赖 UID：空 UID 会让所有账号塌缩成同一个 key——那正是本条要防的「会话跨账号」。
func accountKey(a *auth.Auth) string {
	if a == nil {
		return "anon"
	}
	if u := strings.TrimSpace(a.UID); u != "" {
		return u
	}
	if n := strings.TrimSpace(a.Nickname); n != "" {
		return n
	}
	// 兜底：用 access token 的指纹区分账号（**不落盘、不出日志**，仅作内存 key，
	// 且不进入任何发给上游的头）。
	if t := accessToken(a); t != "" {
		sum := sha256.Sum256([]byte(t))
		return "tok-" + hex.EncodeToString(sum[:4])
	}
	return "anon"
}

// sessionFor 返回 key 对应的稳定会话 ID（X-RACCOON-Session-ID 的取值）。
//
// 形态对齐官方 `sess-<进程内序号>-<8 位 hex>`（实测 sess-0-53927d74 / sess-1-798ffb2b）。
// 本渠道此前是**每请求新建** `sess-<32 位 hex>`，形态与语义两处都不对：
// 同一账号在几分钟内冒出上百个「新会话」，而上游对正常客户端的认知是「一个会话持续翻页」。
// 这是 429 的触发条件之一（R91 取证见 docs/DECISIONS.md）。
//
// 三个维度都满足才新建：key（账号 + 模型）**未命中** 且（首次出现 **或** 已过
// 24h 轮换期）。三者缺一就沿用原会话 ID —— 换号只换该号自己的会话，不会跨账号复用；
// 同一账号同一模型在 24h 内无论多少次请求都保持同一个 ID。
//
// 并发安全：多个请求可能同时首访同一 key，必须持锁查表建表。
func sessionFor(key string) string {
	id, _ := sessionForEx(key)
	return id
}

// sessionForEx 同 sessionFor，但额外返回**本次是否新建**（isNew）。
//
// 供 app.log 留痕用：只打会话 ID 看不出「复用还是新建」，而「三个条件齐才新建」
// 正是本决策要验证的行为——日志里 `new=false` 说明复用了原会话。
func sessionForEx(key string) (id string, isNew bool) {
	now := time.Now()
	sessMu.Lock()
	defer sessMu.Unlock()
	if e, ok := sessByKey[key]; ok && now.Before(e.expires) {
		return e.id, false
	}
	// 会话序号从 0 起（官方第一个会话即 sess-0），每次生成新会话递增。
	id = fmt.Sprintf("sess-%d-%s", sessSeq, randomHex8())
	sessSeq++
	sessByKey[key] = sessionEntry{id: id, expires: now.Add(sessionRotation)}
	return id, true
}

// newTurnID 生成 X-RACCOON-Turn-ID：官方为每轮对话一个 uuid4，本渠道每请求一个。
//
// ⚠️ 已知与官方不一致：官方一个 turn 内所有 LLM 调用共用同一个 turn_id
// （实测某轮 116 次调用全挂同一个 uuid）。本渠道每请求一个，等于「一个会话里 N 个 turn」。
// 本轮只修 session（会话稳定性才是 429 的触发维度），turn 的同类问题留在已知限制里。
func newTurnID() string { return randomUUIDv4() }

// randomHex8 生成 8 位 hex（4 字节随机），对齐官方会话 ID 的 hex 段长度。
func randomHex8() string {
	var b [4]byte
	if _, err := crand.Read(b[:]); err != nil {
		n := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(n >> (i * 8))
		}
	}
	return hex.EncodeToString(b[:])
}

// randomUUIDv4 标准 uuid4 文本形态（`xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx`，8-4-4-4-12）。
//
// ⚠️ 上游同函数用的是 `s[0:4]+"-"+s[4:8]+...` ⇒ 产出 4-4-4-4-16 的畸形串，
// 那不是任何 uuid4 的形态。官方客户端发的是 Python uuid.uuid4() 的标准 36 位，
// 故这里按标准形态生成（用 formatUUID 而不是手切片）。
func randomUUIDv4() string {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		fillFromClock(&b)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return formatUUID(b)
}

// formatUUID 16 字节 → 带连字符的 36 位标准形态（hexdigits 逐字节拼）。
func formatUUID(b [16]byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 36)
	j := 0
	for i, v := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out[j] = '-'
			j++
		}
		out[j] = hexdigits[v>>4]
		out[j+1] = hexdigits[v&0x0f]
		j += 2
	}
	return string(out)
}

// fillFromClock crypto/rand 不可用时的确定性兜底（保持形态，不求熵）。
func fillFromClock(b *[16]byte) {
	n := time.Now().UnixNano()
	for i := 0; i < 8; i++ {
		b[i] = byte(n >> (i * 8))
		b[15-i] = byte(n >> ((7 - i) * 8))
	}
}
