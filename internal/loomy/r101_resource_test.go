// r101_loomy_resource_test.go 锁住 R101：loomy 额度响应读不出余额时必须报错，
// 绝不能返回 (0, nil, nil)。
//
// 为什么必须挡：返回 0 会让调用方把「读不出」当成「余额为 0」——账本记一条 expire，
// 下一轮读到真值再记一条 earn，面板上表现为余额断崖 + 假过期刷屏。上游偶发回
// HTTP 200 但 body 里没有任何余额字段（`{"code":0,"data":{}}` 一类）。
//
// 注意反向约束：**真正的 0 余额仍带字段**（balance=0 / dailyBalance=0），
// 那种情况 items 非空，必须正常返回 0 而不是报错。
package loomy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"omnigate/internal/auth"
)

// loomyPoints 起一个假上游，返回固定 body。
func loomyPoints(t *testing.T, status int, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := New()
	c.Base = srv.URL
	return c
}

// TestUserResourceDetailNoBalanceFieldsIsError HTTP 200 但无任何余额字段 ⇒ 必须报错。
func TestUserResourceDetailNoBalanceFieldsIsError(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"空 data", `{"code":0,"data":{}}`},
		{"空对象", `{}`},
		{"只有 code/msg", `{"code":0,"message":"ok"}`},
		{"data 为 null", `{"code":0,"data":null}`},
		{"余额字段全为 null", `{"code":0,"data":{"balance":null,"dailyBalance":null}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := loomyPoints(t, http.StatusOK, tc.body)
			remain, items, err := c.UserResourceDetail(&auth.Auth{AccessToken: "dummy"})
			if err == nil {
				t.Fatalf("无余额字段时必须报错，却返回 remain=%d items=%d（会被记成假过期）", remain, len(items))
			}
			if !strings.Contains(err.Error(), "缺少余额字段") {
				t.Errorf("错误文案应说明缺少余额字段，得到 %v", err)
			}
		})
	}
}

// TestUserResourceDetailRealZeroBalanceIsOK 真正的 0 余额带字段 ⇒ 正常返回 0，不报错。
func TestUserResourceDetailRealZeroBalanceIsOK(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"balance=0", `{"code":0,"data":{"balance":0}}`},
		{"dailyBalance=0", `{"code":0,"data":{"dailyBalance":0}}`},
		{"两者都 0", `{"code":0,"data":{"balance":0,"dailyBalance":0}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := loomyPoints(t, http.StatusOK, tc.body)
			remain, items, err := c.UserResourceDetail(&auth.Auth{AccessToken: "dummy"})
			if err != nil {
				t.Fatalf("真 0 余额不应报错，得到 %v", err)
			}
			if remain != 0 {
				t.Errorf("remain 应为 0，得到 %d", remain)
			}
			if len(items) == 0 {
				t.Error("真 0 余额也必须产出 items（否则账本无法区分「0」与「读不出」）")
			}
		})
	}
}

// TestUserResourceDetailMalformedJSONIsError 非法 JSON 必须报错，不能静默归 0。
func TestUserResourceDetailMalformedJSONIsError(t *testing.T) {
	c := loomyPoints(t, http.StatusOK, `{"code":0,"data":{`)
	remain, _, err := c.UserResourceDetail(&auth.Auth{AccessToken: "dummy"})
	if err == nil {
		t.Fatalf("非法 JSON 必须报错，却返回 remain=%d", remain)
	}
	if !strings.Contains(err.Error(), "解析失败") {
		t.Errorf("错误文案应说明解析失败，得到 %v", err)
	}
}

// TestUserResourceDetailNonOKIsError 非 200 仍走原有 provider.Error 分支。
func TestUserResourceDetailNonOKIsError(t *testing.T) {
	c := loomyPoints(t, http.StatusForbidden, `{"error":"forbidden"}`)
	if _, _, err := c.UserResourceDetail(&auth.Auth{AccessToken: "dummy"}); err == nil {
		t.Fatal("403 必须报错")
	}
}
