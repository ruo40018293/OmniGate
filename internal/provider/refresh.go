package provider

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"omnigate/internal/auth"
)

// ---------------------------------------------------------------------------
// token 刷新单飞（singleflight）—— R101
// ---------------------------------------------------------------------------
//
// 为什么必须有这一层：部分渠道的 refresh_token 是**单会话**的——上游每次刷新都会轮换它，
// 旧值立刻作废。同一账号的两次刷新一旦并发发出，两者都握着同一个旧 refresh_token，
// 结果必然是一成功一失败（raccoon 报 `200822 refresh_conflict`，qwenwork 报 invalid_grant），
// 失败那次还会把账号推进冷却（默认 10 分钟）甚至判定 session dead，对外表现为
// 「偶发 503 / 账号莫名不可用」。
//
// 这不是理论风险：R101 取证前我们有 7 处 RefreshToken 调用点
// （app.go 的 session 自愈/额度轮询/费率刷新、handler.go 的模型刷新与请求前刷新、
// scheduler.go 的签到刷新与保活批量刷新），彼此没有任何互斥。后台的额度轮询循环
// 与请求路径完全可能在同一秒对同一账号各触发一次刷新。
//
// auth.Auth 自带的 mu 只保证「不会并发读写字段」，拦不住「两次刷新都真的打上游」——
// 串行执行照样各发一次请求，第二个用的还是已被轮换的旧 token。真正需要的是单飞：
// 同一账号同一时刻只放一次真实刷新出去，其余调用等待并共享这次的结果。
//
// 单飞键用 auth 文件路径（见 refreshKey），跨渠道天然隔离，调用方无需再传渠道标识。

// RefreshOnce 对同一账号的并发刷新做单飞。
//
// fn 内部应当**重新检查一次是否仍需要刷新**（用 RefreshIfNeeded 即可，它已经内置）：
// 等待者醒来时 token 可能已经是新的，重检让它直接返回而不是再打一次上游。
//
// 例外是 401 自愈场景：session 已被上游作废而本地 expiresAt 尚未到期，此时不能靠
// NeedsRefresh 判断，那类调用应改用 RefreshForce。
//
// 返回值即本次真实执行的错误，等待者拿到同一个值（含成功）。fn panic 时 defer 会先唤醒
// 所有等待者（附 panic 错误）再把 panic 向上抛，不会让它们永久阻塞。
func RefreshOnce(a *auth.Auth, fn func() error) error {
	return refreshFlights.do(refreshKey(a), fn)
}

// RefreshIfNeeded 是「token 临近过期才刷新」的标准写法，内含单飞与内层重检。
//
// refresh 只负责打上游并把新 token 写进 a（不打日志、不落盘）；落盘由本函数统一做，
// 因为「刷新后必须落盘」是硬约束——refresh token 会轮换，不写回则下次启动用的是旧值，
// 而旧 access token 已被上游作废，本地 expiresAt 又显示有效 ⇒ 卡死在 401。
//
// 返回 nil 有两种语义：真的刷新成功了，或并发的那次已经刷好（本调用什么都没做）。
// 两种情况调用方都可以继续用 a。落盘失败只记日志不返回错误：内存里的 token 已经可用，
// 把它当成刷新失败会让调用方误判账号不可用并进冷却，代价远大于一次写盘失败。
func RefreshIfNeeded(a *auth.Auth, skew time.Duration, refresh func() error) error {
	return RefreshOnce(a, func() error {
		if a == nil {
			return fmt.Errorf("refresh: nil auth")
		}
		if !a.NeedsRefresh(skew) {
			return nil // 已被并发的另一次刷新刷过，或本来就不需要
		}
		return refreshAndSave(a, refresh)
	})
}

// RefreshForce 是 401 自愈专用：无条件刷新（不看本地 expiresAt），同样单飞。
//
// 用于「本地认为 token 还有效、上游却回 401」的场景——此时 NeedsRefresh 为 false，
// 阻塞在 RefreshIfNeeded 上就永远修不回来。
func RefreshForce(a *auth.Auth, refresh func() error) error {
	return RefreshOnce(a, func() error {
		if a == nil {
			return fmt.Errorf("refresh: nil auth")
		}
		return refreshAndSave(a, refresh)
	})
}

// refreshAndSave 真实刷新 + 落盘。调用方必须已持有该 key 的 flight。
func refreshAndSave(a *auth.Auth, refresh func() error) error {
	if err := refresh(); err != nil {
		return err
	}
	if err := a.SaveAtomic(); err != nil {
		// 不返回错误：见 RefreshIfNeeded 的注释。
		log.Printf("refresh save failed uid=%s err=%v", a.UID, err)
	}
	return nil
}

// refreshKey 单飞键。
//
// 以 auth 文件路径为准：它天然唯一（各渠道 loader 各扫各的文件名前缀），且「同一账号在两个
// 目录下各有一份文件」本就代表两个独立的 refresh 会话，理应各自单飞——若改用 UID 反而会
// 把它们错误合并成一次刷新。FilePath 为空（账号尚未落盘）时回落到 UID。
func refreshKey(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	if p := strings.TrimSpace(a.FilePath); p != "" {
		return p
	}
	return strings.TrimSpace(a.UID)
}

// flightGroup 是按 key 的单飞组。map + channel 手写，不引入 golang.org/x/sync 依赖
// （本仓 go.mod 刻意保持极小）。
type flightGroup struct {
	mu    sync.Mutex
	calls map[string]*flightCall
}

// flightCall 一次进行中的刷新。err 在 close(done) 之前写入，close 建立的 happens-before
// 保证等待者读到的一定是最终值。
type flightCall struct {
	done chan struct{}
	err  error
}

var refreshFlights = &flightGroup{calls: make(map[string]*flightCall)}

// do 执行 fn；同一 key 上已有进行中的调用时，等待并共享其结果。
func (g *flightGroup) do(key string, fn func() error) error {
	if key == "" {
		// 既无 FilePath 也无 UID：定位不到账号，无法建 flight 表项。
		// 这类账号本来也落不了盘（SaveAtomic 会报 no FilePath set），直接执行即可。
		return fn()
	}

	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		<-c.done
		return c.err
	}
	c := &flightCall{done: make(chan struct{})}
	g.calls[key] = c
	g.mu.Unlock()

	// 顺序有讲究：
	//   - 先写 err 再 close，等待者才不会读到零值；
	//   - 先 close 再删表项，close 与 delete 之间进来的调用者会看到已完成的 flight 并直接
	//     复用结果，而不是新开一轮重复打上游（那个窗口极小，但正好落在「刷新刚成功、
	//     并发请求刚拿到 401」的交界上，重复刷新就是一次多余的 refresh_conflict）。
	finish := func(err error) {
		c.err = err
		close(c.done)
		g.mu.Lock()
		delete(g.calls, key)
		g.mu.Unlock()
	}

	defer func() {
		if r := recover(); r != nil {
			finish(fmt.Errorf("refresh panic: %v", r))
			panic(r)
		}
	}()

	err := fn()
	finish(err)
	return err
}
