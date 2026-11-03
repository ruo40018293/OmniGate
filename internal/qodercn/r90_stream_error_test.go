package qodercn

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"omnigate/internal/provider"
)

// 本文件锁定 R90 的两条闭环：
//
//	① 上游以 HTTP 200 包裹的业务失败（信封 statusCodeValue>=400）必须归一成
//	  *sseError —— 它实现 provider.StreamErrorClassifier，handler 的 errors.As
//	  能取到 Kind ⇒ 冷却账号 ⇒ 下次请求换号。旧实现把这类帧当「解不出 body」
//	  静默丢弃，客户端界面停在「正在等待模型…」，日志里只剩 180s 后的截断。
//	② 断流必须保留**真实原因**，并区分「我们掐的（空闲超时）」与「上游真断的」。
//
// 口径提醒：出口 code 集合（upstream_truncated / upstream_timeout）不得变，
// internal/gateway 的 streamErrKind 依赖它。

// collectSSE 跑一遍 streamAsOpenAI 并返回写出的全部字节。
func collectSSE(t *testing.T, frames string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	_, err := streamAsOpenAI(&buf, strings.NewReader(frames), "m", func() {}, func() {})
	return buf.String(), err
}

// TestEnvelopeErrorBecomesClassifier 信封 402 必须变成可冷却的流内错误。
func TestEnvelopeErrorBecomesClassifier(t *testing.T) {
	frames := `data: {"statusCodeValue":402,"body":"{\"code\":\"402\",\"message\":\"今日对话次数已达上限\"}"}` + "\n\n"
	out, err := collectSSE(t, frames)

	if err == nil {
		t.Fatalf("信封 402 必须返回错误，否则账号不会被冷却\nout=%s", out)
	}
	var sec provider.StreamErrorClassifier
	if !errors.As(err, &sec) {
		t.Fatalf("错误必须实现 provider.StreamErrorClassifier，否则 handler 取不到 Kind（实测 qwenwork 118 条 402 全部落空）\nerr=%T %v", err, err)
	}
	if got := sec.Kind(); got != provider.ErrHardCredit {
		t.Fatalf("402 应归 ErrHardCredit（额度耗尽），实际 %s", got)
	}
	// 出口帧必须带 code=upstream_quota_exhausted 让客户端能判断「明天再来/去充值」
	if !strings.Contains(out, "upstream_quota_exhausted") {
		t.Errorf("应发出额度耗尽 error 帧\nbody=%s", out)
	}
	if strings.Contains(out, "[DONE]") {
		t.Errorf("失败流不得补 [DONE]（会伪装成正常收尾）\nbody=%s", out)
	}
}

// TestEnvelopeErrorKeepsUpstreamMessage 上游原文必须透出来，不能只剩固定串。
func TestEnvelopeErrorKeepsUpstreamMessage(t *testing.T) {
	frames := `data: {"statusCodeValue":403,"body":"{\"code\":\"403\",\"message\":\"risk control blocked\"}"}` + "\n\n"
	out, err := collectSSE(t, frames)
	if err == nil {
		t.Fatalf("信封 403 必须返回错误\nout=%s", out)
	}
	if !strings.Contains(err.Error(), "risk control blocked") {
		t.Errorf("错误文案应包含上游原文，便于事后定位\nerr=%v", err)
	}
	if !strings.Contains(out, "risk control blocked") {
		t.Errorf("客户端也应看到上游原文\nbody=%s", out)
	}
}

// TestFlatQueueFrameNotSwallowed 排队帧必须仍然交给 onChunk（既有契约，回归保护）。
// 队列不是错误：把它当错误冷却账号，等于排队高峰期把所有号一起打死。
//
// 真实排队帧是三层嵌套：{code:"403", message:"{code:'10605', message:'{isQueued…}'}"}，
// 必须用真实结构，手搓一层会测出假阴性。
func TestFlatQueueFrameNotSwallowed(t *testing.T) {
	frames := "data: " + string(mustJSON(realQueueChunk())) + "\n\n"
	_, err := collectSSE(t, frames)

	var qe *QueueError
	if !errors.As(err, &qe) {
		t.Fatalf("排队帧仍应识别成 *QueueError，实际 %T %v", err, err)
	}
	var sec provider.StreamErrorClassifier
	// 排队帧本身实现了 StreamErrorClassifier（Kind=ErrPassthrough）——这是**正确**的
	// 既有设计：ErrPassthrough 在 handler 里落 default「不罚账号」，只把原因透传给
	// 客户端。R90 不得把它升级成会冷却账号的错误（排队高峰期会把健康号一起打死）。
	if errors.As(err, &sec) && sec.Kind() != provider.ErrPassthrough {
		t.Fatalf("排队帧必须归 ErrPassthrough（不罚账号），实际 kind=%s", sec.Kind())
	}
	if qe.QueueCount != 3058 || qe.WaitSeconds != 185 {
		t.Fatalf("队列信息不得被压扁：%+v", qe)
	}
}

// TestFlatNonAccountFrameStillPassesThrough 110 这类业务码仍走原有直通路径。
func TestFlatNonAccountFrameStillPassesThrough(t *testing.T) {
	frames := `data: {"code":"110","message":"Billing daily count exceeded","model":"deepseek-flash"}` + "\n\n"
	out, _ := collectSSE(t, frames)
	if !strings.Contains(out, `"code":"110"`) {
		t.Fatalf("非账号级扁平帧应原样下传（保留上游 code 字段）\nbody=%s", out)
	}
}

// TestTruncationKeepsRealCause 断流时错误里必须带上真实原因。
//
// 关键时序：半帧是 **EOF 路径**（parseNestedSSE 返回 nil 但 truncated=true），
// 不是读错误路径。旧实现走 truncated 分支时把底层原因整个换成了固定串，
// 用户在生产日志里只能看到「断了」，分不清「上游挂死」「上游主动断」「读到半个帧」。
func TestTruncationKeepsRealCause(t *testing.T) {
	// 复刻真实层叠：一个完整帧之后，第二个 data 行在帧中间被切断
	// （外层 JSON 不闭合 = 上游写到一半就断）。
	head := "data: " + string(mustJSON(map[string]any{
		"body": string(mustJSON(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": "hi"}}},
		})),
	})) + "\n\n"
	frames := head + "data: " + `{"body":"{\"choices\":[{\"delta\":{\"content\":\"半` + "\n\n"
	out, err := collectSSE(t, frames)
	if err == nil {
		t.Fatalf("半帧必须判截断\nout=%s", out)
	}
	if !strings.HasPrefix(err.Error(), "upstream stream truncated") {
		t.Fatalf("必须保留旧前缀（日志/客户端按前缀识别）\nerr=%v", err)
	}
	// 原因**必须真的被带出来**，而不是一句固定串。
	//
	// 关键：半帧是「干净 EOF」，err 为 nil，所以原因只能是 "unknown cause" ——
	// 这恰好证明「有原因的路径」在本测试里覆盖不到。因此单独直接断言
	// TruncationError 对真实错误的行为（下面 TestTruncationErrorKeepsCause）。
	if cause := provider.TruncationCause(err); cause == "" {
		t.Fatalf("原因不得为空串（TruncationCause 必须给出可读描述）")
	}
	// 出口帧同样要带原因，而不是一句固定文案
	if !strings.Contains(out, "upstream_truncated") {
		t.Errorf("应发出截断 error 帧\nbody=%s", out)
	}
	if strings.Contains(out, "[DONE]") {
		t.Errorf("截断不得补 [DONE]\nbody=%s", out)
	}
}

// TestIdleTimeoutReportsItsOwnCode 我们主动掐断（空闲看门狗）必须可区分。
//
// 若与「上游真断」同形，运维看日志永远分不清是网关掐的还是上游断的（R58 的坑）。
func TestIdleTimeoutReportsItsOwnCode(t *testing.T) {
	// idle 看门狗只 cancel context，在途 Read 返回的是 context.Canceled
	if !provider.IsIdleTimeout(context.Canceled) {
		t.Fatalf("context.Canceled 必须被 IsIdleTimeout 认出，否则「我们掐的」与「上游断的」同形")
	}
	if !provider.IsIdleTimeout(provider.ErrIdleTimeout) {
		t.Fatalf("ErrIdleTimeout 必须被认出")
	}
	if got := provider.TruncationCode(provider.ErrIdleTimeout); got != "upstream_timeout" {
		t.Errorf("空闲超时 code=%q want upstream_timeout", got)
	}
	if got := provider.TruncationCode(context.Canceled); got != "upstream_timeout" {
		t.Errorf("context.Canceled code=%q want upstream_timeout", got)
	}
	if got := provider.TruncationCode(errors.New("connection reset by peer")); got != "upstream_truncated" {
		t.Errorf("上游断 code=%q want upstream_truncated", got)
	}
	// 原因文本必须归一，不能把 context.Canceled 原样抛给用户（会被误读成客户端断开）
	if cause := provider.TruncationCause(context.Canceled); strings.Contains(cause, "canceled") {
		t.Errorf("空闲超时原因应归一成看门狗措辞，实际 %q", cause)
	}
}

// TestErrTextOf fallback 链：解不出 message 时不能吞成空串。
func TestErrTextOf(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"code":"402","message":"额度不足"}`, "额度不足"},
		{`{"code":"402"}`, "code=402"},
		{`plain text`, "plain text"},
		{``, "upstream business error"},
	}
	for _, c := range cases {
		if got := errTextOf(c.in); got != c.want {
			t.Errorf("errTextOf(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// TestWriteStreamErrorFrameIsValidSSE 补写的错误帧必须是合法 SSE 单帧。
func TestWriteStreamErrorFrameIsValidSSE(t *testing.T) {
	var buf bytes.Buffer
	writeStreamErrorFrame(&buf, &sseError{status: http.StatusPaymentRequired, msg: "额度耗尽"})
	out := buf.String()
	if !strings.HasPrefix(out, "data: ") || !strings.HasSuffix(out, "\n\n") {
		t.Fatalf("必须是单行 data 帧并以空行结尾\nout=%q", out)
	}
	if strings.Contains(out, "\n\n\n") {
		t.Errorf("不得出现多余空行\nout=%q", out)
	}
}

// TestTruncationErrorKeepsCause 直接锁 provider.TruncationError 的「保留原因」契约。
//
// 为什么必须单独测：上面的端到端用例里半帧是**干净 EOF**（err==nil），只能拿到
// "unknown cause"，覆盖不到「有真实错误时原因是否被保留」这条分支。
func TestTruncationErrorKeepsCause(t *testing.T) {
	orig := errors.New("connection reset by peer")
	wrapped := provider.TruncationError(orig)

	if !strings.HasPrefix(wrapped.Error(), "upstream stream truncated") {
		t.Fatalf("必须保留旧前缀\nerr=%v", wrapped)
	}
	if !strings.Contains(wrapped.Error(), "connection reset by peer") {
		t.Fatalf("必须保住原始错误文本，否则用户还是「只知道断了」\nerr=%v", wrapped)
	}
	if !errors.Is(wrapped, orig) {
		t.Fatalf("必须用 %%w 包装，errors.Is 可穿透\nerr=%v", wrapped)
	}
	if got := provider.TruncationCause(wrapped); !strings.Contains(got, "connection reset") {
		t.Errorf("TruncationCause 应还原原始原因，实际 %q", got)
	}
}
