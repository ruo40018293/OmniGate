package cline

import (
	"net/http"
	"strings"
	"testing"

	"omnigate/internal/auth"
	"omnigate/internal/provider"
)

// R88：本地 knownModel 拒绝必须落成「请求级错误」——不换号、不记账号错误。
//
// 事故模型（上级项目 2026-10-28 实测）：本地拒绝若用 404，Classify 归成 ErrNotFound
// （Rotatable）⇒ handler 认为「这个号不行」去换号 ⇒ 下一个号照样被本地拒 ⇒ 每次轮换
// 再记一笔账号错误 ⇒ 客户端打错一个模型名就能把整条渠道拖进冷却。
func TestLocalUnknownModelIsRequestLevelError(t *testing.T) {
	c := &Client{}
	c.knownIDs = map[string]string{"cline-known-model": "Known"}

	body := []byte(`{"model":"cline-typo-model","messages":[{"role":"user","content":"hi"}]}`)
	_, status, respBody, err := c.ChatStream(&auth.Auth{UID: "u1"}, body)
	if err != nil {
		t.Fatalf("本地拒绝不应返回 transport error：%v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("本地拒绝状态码必须是 400，得到 %d（404 会被归成 ErrNotFound 触发换号+计错）", status)
	}
	if !strings.Contains(string(respBody), "cline-typo-model") {
		t.Fatalf("响应体应指名被拒的模型，实际：%s", respBody)
	}

	kind := Classify(status, string(respBody))
	if kind != provider.ErrBadParams {
		t.Fatalf("应归类为 ErrBadParams，实际 %s", kind)
	}
	if kind.Rotatable() {
		t.Fatal("本地拒绝不得可轮换：换号必然复现，且每次轮换都给健康账号记一笔错误")
	}
}

// 回归：真实的**上游** 404 仍按 ErrNotFound 处理（本次改动只针对本地判定）。
func TestUpstream404RemainsNotFound(t *testing.T) {
	body := `{"error":{"message":"please check the model you provided"}}`
	if kind := Classify(http.StatusNotFound, body); kind != provider.ErrNotFound {
		t.Fatalf("上游 404 应保持 ErrNotFound，实际 %s", kind)
	}
}
