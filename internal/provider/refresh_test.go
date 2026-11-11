// refresh_test.go 锁住 R101 的 token 刷新单飞。
//
// 为什么必须测：部分渠道的 refresh_token 是单会话的，上游每次刷新都轮换它、旧值立即作废。
// 同一账号的两次刷新一旦并发，两者都握着同一个旧值，结果必然一成功一失败
// （raccoon 报 200822 refresh_conflict，qwenwork 报 invalid_grant），失败那次还会把账号
// 推进冷却甚至判 session dead —— 对外表现为「偶发 503 / 账号莫名不可用」。
//
// auth.Auth 自带的 mu 只防字段并发读写，拦不住「两次刷新都真的打上游」——串行执行照样
// 各发一次。真正要锁的是「真实刷新只发生一次，其余调用等待并共享结果」。
package provider

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"omnigate/internal/auth"
)

// newTestAuth 造一个已落盘可 SaveAtomic 的账号（FilePath 指向临时目录）。
func newTestAuth(t *testing.T, uid string) *auth.Auth {
	t.Helper()
	return &auth.Auth{
		UID:         uid,
		AccessToken: "old-access",
		ExpiresAt:   time.Now().Add(-time.Hour).Unix(), // 已过期 ⇒ NeedsRefresh 恒真
		FilePath:    filepath.Join(t.TempDir(), "auth-"+uid+".json"),
	}
}

// TestRefreshOnceCoalescesConcurrentCalls 同一账号的并发刷新只打一次上游，其余共享结果。
func TestRefreshOnceCoalescesConcurrentCalls(t *testing.T) {
	a := newTestAuth(t, "u1")

	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})

	fn := func() error {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release // 卡住首个调用，制造真实的并发窗口
		return nil
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			errs[i] = RefreshOnce(a, fn)
		}(i)
	}

	<-started
	// 等到所有 goroutine 都进到 do() 里（要么持 flight，要么在 <-done 上等）。
	// 这里靠短暂睡眠代替可观测屏障：断言的是最终计数，不是时序。
	time.Sleep(80 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("refresher 应只被调用 1 次，实际 %d 次（单飞失效 ⇒ 会打出 refresh_conflict）", got)
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("第 %d 个等待者拿到了错误: %v（应共享首个调用的成功结果）", i, err)
		}
	}
}

// TestRefreshOncePropagatesError 首个调用失败时，所有等待者拿到同一个错误。
func TestRefreshOncePropagatesError(t *testing.T) {
	a := newTestAuth(t, "u2")
	wantErr := errors.New("refresh_conflict")

	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	fn := func() error {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return wantErr
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var e1, e2 error
	go func() { defer wg.Done(); e1 = RefreshOnce(a, fn) }()
	<-started
	go func() { defer wg.Done(); e2 = RefreshOnce(a, fn) }()
	time.Sleep(80 * time.Millisecond)
	close(release)
	wg.Wait()

	if calls.Load() != 1 {
		t.Fatalf("失败也该只打一次上游，实际 %d 次", calls.Load())
	}
	if !errors.Is(e1, wantErr) || !errors.Is(e2, wantErr) {
		t.Fatalf("两个调用都应拿到同一个错误，得到 %v / %v", e1, e2)
	}
}

// TestRefreshOnceSerializesAfterCompletion 首个 flight 结束后，新的调用重新打上游。
// 单飞只管「同时在途的那一次」，不做结果缓存——token 会再次过期，缓存会锁死旧值。
func TestRefreshOnceSerializesAfterCompletion(t *testing.T) {
	a := newTestAuth(t, "u3")
	var calls atomic.Int32
	fn := func() error { calls.Add(1); return nil }

	if err := RefreshOnce(a, fn); err != nil {
		t.Fatalf("第 1 次: %v", err)
	}
	if err := RefreshOnce(a, fn); err != nil {
		t.Fatalf("第 2 次: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("顺序调用的两次都应真的执行，实际 %d 次", got)
	}
}

// TestRefreshOnceDifferentKeysAreIndependent 不同账号互不阻塞、各自刷新。
func TestRefreshOnceDifferentKeysAreIndependent(t *testing.T) {
	a1 := newTestAuth(t, "u4")
	a2 := newTestAuth(t, "u5")
	var c1, c2 atomic.Int32

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = RefreshOnce(a1, func() error { c1.Add(1); return nil }) }()
	go func() { defer wg.Done(); _ = RefreshOnce(a2, func() error { c2.Add(1); return nil }) }()
	wg.Wait()

	if c1.Load() != 1 || c2.Load() != 1 {
		t.Fatalf("不同账号应各刷新一次，得到 %d / %d", c1.Load(), c2.Load())
	}
}

// TestRefreshOnceNoKeyRunsDirectly 既无 FilePath 也无 UID 时无处建 flight，直接执行。
func TestRefreshOnceNoKeyRunsDirectly(t *testing.T) {
	var calls atomic.Int32
	fn := func() error { calls.Add(1); return nil }
	for i := 0; i < 3; i++ {
		if err := RefreshOnce(&auth.Auth{}, fn); err != nil {
			t.Fatalf("无 key: %v", err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("无 key 时不做单飞，应执行 3 次，实际 %d", calls.Load())
	}
	if err := RefreshOnce(nil, fn); err != nil {
		t.Fatalf("nil auth: %v", err)
	}
	if calls.Load() != 4 {
		t.Fatalf("nil auth 也应直接执行，实际 %d", calls.Load())
	}
}

// TestRefreshOnceRecoversFromPanic panic 不能让等待者永久阻塞。
func TestRefreshOnceRecoversFromPanic(t *testing.T) {
	a := newTestAuth(t, "u6")
	started := make(chan struct{})
	release := make(chan struct{})
	fn := func() error {
		close(started)
		<-release
		panic("boom")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)                // panic 展开时也要收尾，否则本测试自己会永久阻塞
		defer func() { _ = recover() }() // 首个调用者自己接住 panic
		_ = RefreshOnce(a, fn)
	}()
	<-started
	waiter := make(chan error, 1)
	go func() { waiter <- RefreshOnce(a, func() error { return nil }) }()
	time.Sleep(80 * time.Millisecond)
	close(release)

	select {
	case err := <-waiter:
		if err == nil {
			t.Fatal("等待者应拿到 panic 包装的错误，而不是 nil")
		}
		if !strings.Contains(err.Error(), "refresh panic") {
			t.Fatalf("错误应说明是 panic，得到 %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("panic 后等待者被永久阻塞（flight 未 close）")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("首个调用者未正常收尾")
	}
}

// TestRefreshIfNeededSkipsWhenFresh 未临近过期时一次上游都不打。
func TestRefreshIfNeededSkipsWhenFresh(t *testing.T) {
	a := &auth.Auth{UID: "u7", ExpiresAt: time.Now().Add(24 * time.Hour).Unix()}
	var calls atomic.Int32
	err := RefreshIfNeeded(a, time.Minute, func() error { calls.Add(1); return nil })
	if err != nil {
		t.Fatalf("RefreshIfNeeded: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("token 还新鲜时不应刷新，实际 %d 次", calls.Load())
	}
}

// TestRefreshIfNeededRefreshesAndSaves 过期时刷新并把新 token 落盘。
func TestRefreshIfNeededRefreshesAndSaves(t *testing.T) {
	a := newTestAuth(t, "u8")
	var calls atomic.Int32
	err := RefreshIfNeeded(a, time.Minute, func() error {
		calls.Add(1)
		a.AccessToken = "new-access"
		a.ExpiresAt = time.Now().Add(24 * time.Hour).Unix()
		return nil
	})
	if err != nil {
		t.Fatalf("RefreshIfNeeded: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("应刷新 1 次，实际 %d", calls.Load())
	}
	// 落盘必须发生：不写回则下次启动用旧 refresh_token（已被上游轮换作废）⇒ 卡死 401。
	raw, readErr := os.ReadFile(a.FilePath)
	if readErr != nil {
		t.Fatalf("应已落盘，读取失败: %v", readErr)
	}
	if !strings.Contains(string(raw), "accessToken") {
		t.Fatalf("落盘内容不含 accessToken 字段: %s", raw)
	}
}

// TestRefreshIfNeededInnerRecheck 等待者醒来后重检，token 已是新的就不再打上游。
func TestRefreshIfNeededInnerRecheck(t *testing.T) {
	a := newTestAuth(t, "u9")

	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	first := func() error {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		a.AccessToken = "fresh"
		a.ExpiresAt = time.Now().Add(24 * time.Hour).Unix()
		return nil
	}
	second := func() error { calls.Add(1); return nil }

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = RefreshIfNeeded(a, time.Minute, first) }()
	<-started
	go func() { defer wg.Done(); _ = RefreshIfNeeded(a, time.Minute, second) }()
	time.Sleep(80 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("等待者应重检发现已刷新、不再打上游，实际共 %d 次", got)
	}
}

// TestRefreshForceIgnoresExpiry 401 自愈场景：本地 expiresAt 显示有效也强制刷新。
func TestRefreshForceIgnoresExpiry(t *testing.T) {
	a := &auth.Auth{
		UID:         "u10",
		AccessToken: "looks-valid",
		ExpiresAt:   time.Now().Add(24 * time.Hour).Unix(),
		FilePath:    filepath.Join(t.TempDir(), "auth-u10.json"),
	}
	var calls atomic.Int32
	err := RefreshForce(a, func() error { calls.Add(1); return nil })
	if err != nil {
		t.Fatalf("RefreshForce: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("RefreshForce 必须无视 expiresAt 强制执行，实际 %d 次", calls.Load())
	}
}

// TestRefreshKeyPrefersFilePath 单飞键优先用文件路径（跨渠道天然隔离），无路径回落 UID。
func TestRefreshKeyPrefersFilePath(t *testing.T) {
	if got := refreshKey(&auth.Auth{FilePath: "/tmp/a.json", UID: "u"}); got != "/tmp/a.json" {
		t.Fatalf("应优先 FilePath，得到 %q", got)
	}
	if got := refreshKey(&auth.Auth{UID: "u"}); got != "u" {
		t.Fatalf("无 FilePath 应回落 UID，得到 %q", got)
	}
	if got := refreshKey(nil); got != "" {
		t.Fatalf("nil auth 应返回空键，得到 %q", got)
	}
}
