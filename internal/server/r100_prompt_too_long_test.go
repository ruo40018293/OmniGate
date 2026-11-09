// r100_prompt_too_long_test.go 锁住 R100：minimax 的「上下文超窗」必须归
// ErrPromptTooLong，而不是可轮转的 ErrClient。
//
// 事故（2026-11-09 16:00:55 / 16:01:00 生产日志两连）：
//
//	rotate platform=minimax uid=8b0948c077d2bcf1 kind=client status=400
//	body={"type":"error","error":{"type":"invalid_request_error",
//	     "message":"invalid params, context window exceeds limit (2013)"},"request_id":"0740b…"}
//	access POST /v1/chat/completions status=400 bytes=169 dur=3.058s
//
// 上游这句话里的 2013 只出现在 message 文本尾部（`(2013)`），**不是**
// `"code":2013` 字段形态，所以 provider.CodeMarker 看不见它；于是 Classify 落到
// ErrClient，而 ErrClient 在 Rotatable 白名单里 —— handler 白烧一次轮转（本机每渠道
// 只有 1 个账号，必然失败），日志还把它记成 kind=client，读起来像「这个号坏了」。
//
// 本测试用**真实的 minimax.Classify** 串起整条链，断言：
//  1. 上游只被调用 1 次（不换号）；
//  2. 上游原文透传给客户端；
//  3. 两个账号都不累计错误计数、不被冷却。
package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"omnigate/internal/auth"
	"omnigate/internal/minimax"
	"omnigate/internal/pool"
	"omnigate/internal/provider"
)

// r100Body 是生产日志里逐字复制的上游 400 原文。
const r100Body = `{"type":"error","error":{"type":"invalid_request_error",` +
	`"message":"invalid params, context window exceeds limit (2013)"},` +
	`"request_id":"0740b3a3-2a8c-4f56-9d7e-6cf9e9f0e111"}`

// r100Upstream 两个账号、固定返回 400 + 上游原文，Classify 用真实 minimax 实现。
type r100Upstream struct {
	mu    sync.Mutex
	calls []string
}

func (u *r100Upstream) RefreshToken(*auth.Auth) error                        { return nil }
func (u *r100Upstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) { return nil, nil }
func (u *r100Upstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}
func (u *r100Upstream) UserResource(*auth.Auth) (int64, error) { return 0, nil }
func (u *r100Upstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (u *r100Upstream) DailyCheckin(*auth.Auth) error { return nil }

// 关键：走真实实现，不在这里重新描述判据——否则测的是测试自己。
func (u *r100Upstream) Classify(status int, body string) provider.ErrKind {
	return minimax.Classify(status, body)
}

func (u *r100Upstream) ChatStream(a *auth.Auth, _ []byte) (io.ReadCloser, int, []byte, error) {
	u.mu.Lock()
	u.calls = append(u.calls, a.UID)
	u.mu.Unlock()
	return nil, http.StatusBadRequest, []byte(r100Body), nil
}
func (u *r100Upstream) Stream(http.ResponseWriter, io.Reader, string) (map[string]any, error) {
	return nil, nil
}
func (u *r100Upstream) Aggregate(io.Reader, string) (map[string]any, error) {
	return map[string]any{"choices": []any{}}, nil
}

func (u *r100Upstream) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.calls)
}

func newR100Handler(t *testing.T, up *r100Upstream) (*Handler, *pool.Pool) {
	t.Helper()
	p := pool.New(t.TempDir() + "/state.json")
	p.Add(&auth.Auth{Kind: string(provider.Minimax), AccessToken: "at-A", ExpiresAt: 4102444800, UID: "mmA"})
	p.Add(&auth.Auth{Kind: string(provider.Minimax), AccessToken: "at-B", ExpiresAt: 4102444800, UID: "mmB"})
	// A 积分更高保证首挑确定性（PickExcluding 的 map 遍历顺序是随机的）。
	p.SetCreditDetail("mmA", 1000, 0, 0)
	p.SetCreditDetail("mmB", 500, 0, 0)
	return NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.Minimax: {
				Kind:         provider.Minimax,
				Pool:         p,
				Upstream:     up,
				StaticModels: []provider.ModelInfo{{ID: "MiniMax-M3"}},
			},
		},
		HardCooldown: time.Hour,
		SoftCooldown: time.Minute,
		ErrThreshold: 1, // 哪怕计一次错也会立刻冷却，测试立刻能看见
		ErrCooldown:  time.Minute,
		MaxRotate:    3,
	}), p
}

// TestContextWindowExceededDoesNotRotate 是 R100 的核心闸门。
func TestContextWindowExceededDoesNotRotate(t *testing.T) {
	up := &r100Upstream{}
	h, p := newR100Handler(t, up)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"minimax/MiniMax-M3","stream":false,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，want 400（上游原文透传）\n响应: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "context window exceeds limit") {
		t.Fatalf("应原样透传上游原文，实际: %s", rec.Body.String())
	}
	if n := up.callCount(); n != 1 {
		t.Fatalf("上游被调用 %d 次，want 1 —— 上下文超窗是请求级错误，换号必然复现", n)
	}
	for _, uid := range []string{"mmA", "mmB"} {
		st, _ := p.Status(uid)
		if st.Cooling || st.Disabled {
			t.Errorf("uid=%s 被惩罚（cooling=%t disabled=%t）——请求级错误不许罚号",
				uid, st.Cooling, st.Disabled)
		}
		if st.ErrCount != 0 {
			t.Errorf("uid=%s errCount=%d，want 0", uid, st.ErrCount)
		}
	}
}

// TestOtherMinimax4xxStillRotates 反向守卫：不能为了修这一条把所有 4xx 都变成不轮转。
// 一个与账号权益相关的普通 4xx 仍应保留 ErrClient 的可轮转语义。
func TestOtherMinimax4xxStillRotates(t *testing.T) {
	if got := minimax.Classify(http.StatusBadRequest, `{"error":{"message":"some other problem"}}`); got != provider.ErrClient {
		t.Fatalf("普通 400 仍应是 ErrClient（可换一次号），实得 %v", got)
	}
	if !provider.ErrClient.Rotatable() {
		t.Fatal("ErrClient 必须在 Rotatable 白名单里，否则本测试失去意义")
	}
}
