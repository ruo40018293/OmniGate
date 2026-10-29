package app

import (
	"encoding/json"
	"net/http"
	"testing"

	"omnigate/internal/provider"
)

// TestInUseEndpoint 「调用中」轻端点（R73）。
//
// 三条契约：① 挂在会话守卫 mux 上（匿名 401，不能裸奔）；② in_use 显式为 false
// 而不是省略（前端布尔判断靠它，缺字段与 false 必须可区分）；③ 打点后 in_use
// 变 true 并带 last 时间 —— 徽标在「调用中」消失后要靠 last 落回「最近用过」。
func TestInUseEndpoint(t *testing.T) {
	a := newPanelApp(t, "0.0.0.0", "s3cret-pass")
	mux := panelMux(a)

	if w := doReq(mux, "GET", "/api/inuse", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("匿名 /api/inuse 应 401，实际 %d", w.Code)
	}

	lw := doReq(mux, "POST", "/api/auth/login", `{"password":"s3cret-pass"}`, nil)
	ck := lw.Result().Cookies()[0]

	get := func() map[string]map[string]any {
		t.Helper()
		w := doReq(mux, "GET", "/api/inuse", "", ck)
		if w.Code != http.StatusOK {
			t.Fatalf("/api/inuse 应 200，实际 %d: %s", w.Code, w.Body.String())
		}
		var body struct {
			Accounts map[string]map[string]any `json:"accounts"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("JSON 解析失败: %v（%s）", err, w.Body.String())
		}
		return body.Accounts
	}

	rows := get()
	row, ok := rows["wb-1"]
	if !ok {
		t.Fatalf("应含 wb-1，实际 %v", rows)
	}
	if v, exists := row["in_use"]; !exists || v != false {
		t.Fatalf("in_use 必须显式为 false，实际=%v 存在=%v", v, exists)
	}
	if _, exists := row["last"]; exists {
		t.Fatalf("从未调用过不应带 last，实际 %v", row["last"])
	}

	rt := a.runtime(provider.WorkBuddy)
	rt.Pool.NotifyUsed("wb-1")

	row = get()["wb-1"]
	if row["in_use"] != true {
		t.Fatalf("打点后 in_use 应为 true，实际 %v", row["in_use"])
	}
	if last, _ := row["last"].(string); last == "" {
		t.Fatal("打点后应带 last 时间（徽标要靠它落回「最近用过」）")
	}
}
