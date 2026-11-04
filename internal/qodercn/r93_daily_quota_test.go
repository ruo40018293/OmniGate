package qodercn

import (
	"testing"

	"omnigate/internal/provider"
)

// TestDailyQuotaIsNotARequestError 锁定 R93 的核心分类结论。
//
// 2026-11-04 生产实测（app.log）：
//
//	qodercn err=upstream 403: Billing daily count exceeded
//
// 改前：403 + 非空 body 走到 Classify 末尾的 `return provider.ErrClient`。
// ErrClient 在 ErrKind.Rotatable() 白名单里 ⇒ handler 换号 + NoteError 记一笔；
// 而本机每个渠道只有 1 个账号，换号必败，最后一次换号失败后返回一个
// 毫无信息量的 503。用户看到的只是「停了」。
//
// 正确语义：日额度耗尽 = 权益问题（ErrHardCredit），应当冷却到日重置，
// 而不是当成「这个请求有问题，换个号再试」。
func TestDailyQuotaIsNotARequestError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"实测原文", 403, `{"code":"403","message":"Billing daily count exceeded"}`},
		{"无 Billing 前缀", 403, `{"message":"Daily count exceeded"}`},
		{"中文上限文案", 403, `{"message":"您今天的对话次数已达上限，可明天继续"}`},
		{"200 但 body 是额度文案", 200, `{"code":403,"message":"billing daily count exceeded"}`},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != provider.ErrHardCredit {
			t.Errorf("%s: Classify(%d, %q) = %v, want ErrHardCredit", c.name, c.status, c.body, got)
		}
	}
}

// TestDailyQuotaKeepsNeighbouringSemantics 防止为修这一类而误伤旁边的判定：
// 403 空 body 仍是 WAF 拦截、普通 403 仍是请求级错误、402 仍是权益不足。
func TestDailyQuotaKeepsNeighbouringSemantics(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   provider.ErrKind
	}{
		{"403 空 body 仍判 WAF", 403, "  ", provider.ErrWafBlock},
		{"普通 403 仍是请求级错误", 403, `{"message":"forbidden by policy"}`, provider.ErrClient},
		{"402 仍是权益不足", 402, `{"message":"payment required"}`, provider.ErrHardCredit},
		{"429 仍是软限流", 429, `{"message":"too many requests"}`, provider.ErrSoftRate},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("%s: Classify(%d, %q) = %v, want %v", c.name, c.status, c.body, got, c.want)
		}
	}
}
