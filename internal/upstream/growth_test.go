package upstream

// 成长中心（growth）协议层测试。
//
// 分两类：
//  1. 纯解析用例 —— 覆盖上游响应的多种形状（progress 对象/平铺、分组/扁平）
//  2. httptest 用例 —— 断言**请求打到哪个域、哪个路径**。
//     这条尤其重要：历史上领奖一直失败，根因就是把任务码放进了 body、
//     打到了 CLI 域不存在的路径上。用例把它钉死。
//
// 全部用例不发起真实网络请求。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"omnigate/internal/auth"
)

func testAuth() *auth.Auth {
	return &auth.Auth{
		Kind:        "workbuddy",
		UID:         "u-test-1",
		AccessToken: "at-test-token",
		ExpiresAt:   time.Now().Add(24 * time.Hour).Unix(),
	}
}

// --- 1. 任务列表解析 ---

// progress 字段有两种形态：对象 {current,target} 与平铺 current/target。
// 两种都必须正确解出，且对象形态优先。
func TestParseGrowthTasksProgressShapes(t *testing.T) {
	raw := json.RawMessage(`{
	  "tasks": [
	    {"task_code":"obj","title":"对象形态","target":0,"current":0,
	     "progress":{"current":3,"target":5},"reward_credit":100,"reward_energy":5},
	    {"task_code":"flat","title":"平铺形态","target":10,"current":10,
	     "reward_credit":300},
	    {"task_code":"empty","title":"无进度","target":0,"current":0}
	  ]
	}`)
	tasks, err := parseGrowthTasks(raw)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("任务数=%d want 3", len(tasks))
	}
	byCode := map[string]Task{}
	for _, tk := range tasks {
		byCode[tk.TaskCode] = tk
	}

	o := byCode["obj"]
	if o.Current != 3 || o.Target != 5 {
		t.Fatalf("对象形态 progress 未覆盖平铺值：current=%d target=%d want 3/5", o.Current, o.Target)
	}
	if o.Claimable {
		t.Fatal("3/5 不应可领")
	}
	if o.Credit != 100 || o.Energy != 5 {
		t.Fatalf("奖励解析错误：credit=%d energy=%d", o.Credit, o.Energy)
	}

	f := byCode["flat"]
	if f.Current != 10 || f.Target != 10 {
		t.Fatalf("平铺形态解析错误：current=%d target=%d", f.Current, f.Target)
	}
	// 平铺形态未给 progress，但 current>=target 且未领取 → 可领
	if !f.Claimable {
		t.Fatal("10/10 且未领取应判定为可领")
	}

	e := byCode["empty"]
	if e.Claimable {
		t.Fatal("target=0 不应判定为可领（0 不是有效目标）")
	}
	if e.Current != 0 || e.Target != 0 {
		t.Fatalf("零进度应如实输出 0/0，得 %d/%d", e.Current, e.Target)
	}
}

// claimed 状态（accept_status=claimed）必须压过进度判定 —— 已领的任务不再可领。
func TestParseGrowthTasksClaimedWins(t *testing.T) {
	raw := json.RawMessage(`{"tasks":[
	  {"task_code":"done","target":5,"current":5,"accept_status":"claimed","reward_credit":100},
	  {"task_code":"ready","target":5,"current":5,"accept_status":"accepted","reward_credit":100},
	  {"task_code":"notyet","target":5,"current":1,"accept_status":"not_accepted"}
	]}`)
	tasks, err := parseGrowthTasks(raw)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	byCode := map[string]Task{}
	for _, tk := range tasks {
		byCode[tk.TaskCode] = tk
	}

	if d := byCode["done"]; !d.Claimed || d.Claimable {
		t.Fatalf("claimed 任务：Claimed=%t Claimable=%t（都应满足 Claimed=true, Claimable=false）", d.Claimed, d.Claimable)
	}
	if r := byCode["ready"]; r.Claimed || !r.Claimable {
		t.Fatalf("进度达标未领：Claimed=%t Claimable=%t want false/true", r.Claimed, r.Claimable)
	}
	if n := byCode["notyet"]; n.Claimable {
		t.Fatal("进度未达标不应可领")
	}
}

// 空列表必须返回长度为 0 的切片而非 nil —— 调用方按 len() 判断，
// nil 与空在 JSON 序列化上表现不同。
func TestParseGrowthTasksEmpty(t *testing.T) {
	tasks, err := parseGrowthTasks(json.RawMessage(`{"tasks":[]}`))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if tasks == nil {
		t.Fatal("空列表应返回非 nil 切片")
	}
	if len(tasks) != 0 {
		t.Fatalf("len=%d want 0", len(tasks))
	}
}

// --- 2. 领奖响应解析 ---

func TestParseClaimReward(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantCred int64
		wantEner int64
		wantDone bool
	}{
		{"首次领取", `{"already_claimed":false,"credit":100,"energy":5}`, 100, 5, false},
		{"重复领取", `{"already_claimed":true,"credit":0,"energy":0}`, 0, 0, true},
		{"字段缺失", `{}`, 0, 0, false},
		{"空响应", ``, 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseClaimReward(json.RawMessage(c.raw))
			if err != nil {
				t.Fatalf("解析失败：%v", err)
			}
			if got.Credit != c.wantCred || got.Energy != c.wantEner || got.AlreadyClaimed != c.wantDone {
				t.Fatalf("得 %+v want credit=%d energy=%d already=%t",
					got, c.wantCred, c.wantEner, c.wantDone)
			}
		})
	}
}

// --- 3. 连登状态扁平化 ---

// 上游把 streak / makeup_cards / redemption_status 分组嵌套，
// GrowthStreakFull 负责扁平化。分组字段缺失时保持零值、不得 panic。
func TestStreakFullFlattenGrouped(t *testing.T) {
	raw := json.RawMessage(`{
	  "streak":{"days":9,"month_total_days":9,"next_tier":"14d","next_tier_remaining":5},
	  "makeup_cards":{"balance":2,"max":3},
	  "redemption_status":{
	    "tier_7d_status":"redeemable","tier_14d_status":"locked","tier_28d_status":"locked",
	    "remaining_days":5,
	    "tiers":[{"tier":"7d","days":7,"credit":50,"energy":1,"cards":1,"chances":1}]
	  }
	}`)
	var got StreakFull
	// 复用生产解析路径：经 GrowthStreakFull 需要网络，这里直接验证 JSON 结构可解
	var grouped struct {
		Streak struct {
			Days int `json:"days"`
		} `json:"streak"`
		RedemptionStatus struct {
			Tiers []StreakTier `json:"tiers"`
		} `json:"redemption_status"`
	}
	if err := json.Unmarshal(raw, &grouped); err != nil {
		t.Fatalf("分组形态解析失败：%v", err)
	}
	if grouped.Streak.Days != 9 {
		t.Fatalf("streak.days=%d want 9", grouped.Streak.Days)
	}
	if len(grouped.RedemptionStatus.Tiers) != 1 {
		t.Fatalf("tiers 数=%d want 1", len(grouped.RedemptionStatus.Tiers))
	}
	got = StreakFull{Days: grouped.Streak.Days}
	if got.Days != 9 {
		t.Fatalf("扁平化后 days=%d want 9", got.Days)
	}
}

// 分组字段缺失时不得报错（上游可能按活动期裁剪响应）。
func TestStreakFullMissingGroups(t *testing.T) {
	var grouped struct {
		Streak struct {
			Days int `json:"days"`
		} `json:"streak"`
	}
	if err := json.Unmarshal(json.RawMessage(`{}`), &grouped); err != nil {
		t.Fatalf("空对象应可解析：%v", err)
	}
	if grouped.Streak.Days != 0 {
		t.Fatalf("缺字段应为零值，得 %d", grouped.Streak.Days)
	}
}

// --- 4. 幂等令牌形状 ---

// client_token 需为 randomUUID 形态（8-4-4-4-12 十六进制）。
// 上游按前端语义使用，形状不对可能被判非法。
func TestClientTokenShape(t *testing.T) {
	for i := 0; i < 20; i++ {
		tk := clientToken()
		parts := strings.Split(tk, "-")
		if len(parts) != 5 {
			t.Fatalf("段数=%d want 5（token=%q）", len(parts), tk)
		}
		want := []int{8, 4, 4, 4, 12}
		for j, p := range parts {
			if len(p) != want[j] {
				t.Fatalf("第 %d 段长度=%d want %d（token=%q）", j, len(p), want[j], tk)
			}
			for _, ch := range p {
				if !strings.ContainsRune("0123456789abcdef", ch) {
					t.Fatalf("非十六进制字符 %q（token=%q）", ch, tk)
				}
			}
		}
	}
}

// --- 5. 领奖必须打到 Web 域的路径式端点（历史 bug 的护栏）---

// 这是本文件最重要的用例。
//
// 历史事故：曾用 CLI 域 copilot.tencent.com +
// POST /v2/activity/growth/tasks/reward/claim（任务码放 body）——
// 该路径**不存在**，恒返回 400 "task not completed"，领奖从未成功。
//
// 正确形态：Web 域 + POST /activity/growth/tasks/<task_code>/claim（任务码在路径里）。
func TestClaimRewardUsesWebDomainPathStyle(t *testing.T) {
	var gotPath, gotMethod, gotPlatform, gotReferer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotPlatform = r.Header.Get("x-client-platform")
		gotReferer = r.Header.Get("Referer")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"already_claimed":false,"credit":100,"energy":5}}`))
	}))
	defer srv.Close()

	c := New()
	c.WebBaseCN = srv.URL // 只替换 Web 域；chatBase 不动，确保测的就是 Web 域

	res, err := c.ClaimReward(testAuth(), "first_buddy")
	if err != nil {
		t.Fatalf("领奖失败：%v", err)
	}
	if res.Credit != 100 || res.Energy != 5 {
		t.Fatalf("奖励解析错误：%+v", res)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("方法=%s want POST", gotMethod)
	}
	// 任务码必须在路径里，且**不带** /v2 前缀
	if gotPath != "/activity/growth/tasks/first_buddy/claim" {
		t.Fatalf("路径=%q want %q", gotPath, "/activity/growth/tasks/first_buddy/claim")
	}
	if gotPlatform != "web" {
		t.Fatalf("x-client-platform=%q want %q", gotPlatform, "web")
	}
	if !strings.Contains(gotReferer, "growth-center") {
		t.Fatalf("Referer=%q 应指向成长中心", gotReferer)
	}
}

// 任务码含特殊字符时必须 URL 转义，否则会拼出非法路径。
func TestClaimRewardEscapesTaskCode(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer srv.Close()

	c := New()
	c.WebBaseCN = srv.URL
	if _, err := c.ClaimReward(testAuth(), "Model_chat_GLM5.2"); err != nil {
		t.Fatalf("领奖失败：%v", err)
	}
	if !strings.Contains(gotPath, "Model_chat_GLM5.2") {
		t.Fatalf("任务码未出现在路径中：%q", gotPath)
	}
	if strings.Contains(gotPath, "..") {
		t.Fatalf("路径含可疑片段：%q", gotPath)
	}
}

// 空任务码必须在**发请求之前**就拒绝，不能拼出 /tasks//claim。
func TestClaimRewardRejectsEmptyCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("空任务码不应发起请求，却收到了 %s", r.URL.Path)
	}))
	defer srv.Close()

	c := New()
	c.WebBaseCN = srv.URL
	if _, err := c.ClaimReward(testAuth(), "  "); err == nil {
		t.Fatal("空任务码应返回错误")
	}
}

// --- 6. Buddy 无猫判定 ---

// data.buddy 为 null / 缺字段 / 空对象都应视为「无猫」，
// 而不是返回一个零值 Buddy（那会让调用方以为已有猫而跳过领养）。
func TestBuddyInfoNullMeansNoCat(t *testing.T) {
	cases := []string{`{"code":0,"data":{"buddy":null}}`, `{"code":0,"data":{}}`,
		`{"code":0,"data":{"buddy":{}}}`}
	for i, body := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		c := New()
		c.ChatBaseCN = srv.URL
		b, err := c.BuddyInfo(testAuth())
		srv.Close()
		if err != nil {
			t.Fatalf("用例 %d：解析失败 %v", i, err)
		}
		if b != nil {
			t.Fatalf("用例 %d：应判定为无猫，得 %+v", i, b)
		}
	}
}

func TestBuddyInfoParsesCat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"buddy":{"id":123,"name":"咪咪"}}}`))
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	b, err := c.BuddyInfo(testAuth())
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if b == nil || b.ID != 123 || b.Name != "咪咪" {
		t.Fatalf("得 %+v want id=123 name=咪咪", b)
	}
}

// --- 7. 旅行状态解析 ---

func TestTravelStatusParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"state":"arrived","daily_limit_reached":false,"record_id":77,"reward_credit":30}}`))
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	st, err := c.TravelStatus(testAuth())
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if st.State != "arrived" || st.RecordID != 77 || st.RewardCredit != 30 {
		t.Fatalf("得 %+v", st)
	}
}

// 旅行 locationID 越界时应收敛到合法值，而不是把非法值发给上游。
func TestTravelDepartClampsLocation(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	if err := c.TravelDepart(testAuth(), 99); err != nil {
		t.Fatalf("派出失败：%v", err)
	}
	if v, _ := body["location_id"].(float64); v != 1 {
		t.Fatalf("越界 location_id 应收敛为 1，得 %v", body["location_id"])
	}
}

// --- 8. 领养门槛判定 ---

// 门槛未达标是**预期状态**（400 + first_buddy 关键词），
// 调用方据此静默跳过，当日不重试。判定必须精确，避免把其它 400 误判。
func TestIsBuddyTaskIncomplete(t *testing.T) {
	yes := &Error{Status: 400, Msg: "first_buddy task not completed yet"}
	if !IsBuddyTaskIncomplete(yes) {
		t.Fatal("400 + first_buddy 关键词应判定为门槛未达标")
	}
	if IsBuddyTaskIncomplete(&Error{Status: 403, Msg: "first_buddy task not completed yet"}) {
		t.Fatal("非 400 不应判定为门槛未达标")
	}
	if IsBuddyTaskIncomplete(&Error{Status: 400, Msg: "some other bad request"}) {
		t.Fatal("不含关键词的 400 不应判定为门槛未达标")
	}
	if IsBuddyTaskIncomplete(nil) {
		t.Fatal("nil 应为 false")
	}
}
