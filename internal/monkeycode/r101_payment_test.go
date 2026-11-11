// r101_payment_test.go 锁住 R101：monkeycode 的 402 Payment Required 必须归
// ErrHardCredit（额度耗尽），不能落进 ErrPassthrough。
//
// 原实现的 switch 里没有 402 分支，于是 402 掉进 `case status >= 400: return
// provider.ErrPassthrough`：既不把账号推进硬冷却，也不触发换号，只把上游原文透传给
// 客户端 —— 用户侧表现是「一直报错、不知道是哪个号没钱了」，面板上那个号还是「健康」。
//
// 上游 rockswang/wild-work 的 25b28c36e 是同一处修复（PR #220）。
package monkeycode

import (
	"net/http"
	"testing"

	"omnigate/internal/provider"
)

// TestClassifyPaymentRequiredIsHardCredit 402 → 硬额度不足。
func TestClassifyPaymentRequiredIsHardCredit(t *testing.T) {
	c := New()
	bodies := []string{
		`{}`,
		`{"error":"payment required"}`,
		`{"error":{"code":"insufficient_quota","message":"quota exceeded"}}`,
		`{"message":"余额不足"}`,
	}
	for _, body := range bodies {
		if got := c.Classify(http.StatusPaymentRequired, body); got != provider.ErrHardCredit {
			t.Errorf("Classify(402, %s) = %v，期望 ErrHardCredit（否则只透传、不冷却不换号）", body, got)
		}
	}
}

// TestClassifyOther4xxStillPassthrough 其余 4xx 仍走透传，不许被 402 的改动带偏。
func TestClassifyOther4xxStillPassthrough(t *testing.T) {
	c := New()
	cases := []struct {
		status int
		body   string
		want   provider.ErrKind
	}{
		{http.StatusForbidden, `{"error":"invalid ohmyagent request"}`, provider.ErrPassthrough},
		{http.StatusRequestEntityTooLarge, `{}`, provider.ErrPassthrough},
		{http.StatusUnprocessableEntity, `{}`, provider.ErrPassthrough},
		{http.StatusUnauthorized, `{"error":"unauthorized"}`, provider.ErrSessionDead},
		{http.StatusTooManyRequests, `{}`, provider.ErrSoftRate},
		{http.StatusBadGateway, `{}`, provider.ErrServer},
		{http.StatusBadRequest, `{"error":{"code":"model_not_found"}}`, provider.ErrBadParams},
	}
	for _, tc := range cases {
		if got := c.Classify(tc.status, tc.body); got != tc.want {
			t.Errorf("Classify(%d, %s) = %v，期望 %v", tc.status, tc.body, got, tc.want)
		}
	}
}

// TestHardCreditIsNotPassthrough 语义守卫：ErrHardCredit 与 ErrPassthrough 必须是不同的 kind。
// 若哪天有人把 402 又并回透传，这条会同时提醒「两者行为完全不同」。
func TestHardCreditIsNotPassthrough(t *testing.T) {
	if provider.ErrHardCredit == provider.ErrPassthrough {
		t.Fatal("ErrHardCredit 与 ErrPassthrough 不得是同一个 kind")
	}
}
