package traework

// TraeWork 签到「换设备号重试」的测试。
//
// ## 为什么这个能力容易被写错
//
// claim 接口是**设备维度**判定，额度发放是**账号维度**。二者不一致时：
//
//	9095「当前设备今日已签」≠ 这个账号今天领过了
//
// 2026-10-12 真实事故：两个账号共用设备号 44842564…，
// 先签的用掉当天额度，后签的连撞 9095，100 积分没领到。
//
// 另一个陷阱：**code==0 不代表入账**。已领过的账号换号后 claim 仍返回 0，
// 但额度不变。若以 code==0 判成功，会把「今天已领」误报成「刚领到」。
//
// 全部用例不发起真实网络请求。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"omnigate/internal/auth"
)

// rotateFixture 模拟一个「多账号共用设备号」的上游。
type rotateFixture struct {
	mu sync.Mutex
	// usedDevices 已签到过的设备号集合（设备维度判定的核心）
	usedDevices map[string]bool
	// claimedAccounts 已领过额度的账号（账号维度）
	claimedAccounts map[string]bool
	// remain 当前账号的额度（每个 uid 一个）
	remain map[string]int64
	// throttleFirstN 前 N 次请求返回 9074（模拟瞬时限流）
	throttleFirstN int
	// alwaysZero 为 true 时，任何设备号都返回 code=0 但额度不涨
	//（模拟「今天确实已领过」—— code==0 并不代表入账）
	alwaysZero bool
	// claimHits claim 端点被调用次数
	claimHits int
	// deviceSeq 按调用顺序记录每次用的设备号（含 ent-usage 查询）
	deviceSeq []string
	// claimDevices 只记录 claim 端点用过的设备号（判定「是否换号」必须用它）
	claimDevices []string
	// uidSeq 按调用顺序记录每次的 uid
	uidSeq []string
}

func newRotateFixture() *rotateFixture {
	return &rotateFixture{
		usedDevices:     map[string]bool{},
		claimedAccounts: map[string]bool{},
		remain:          map[string]int64{},
	}
}

// ServeHTTP 实现一个最小上游：status / claim / ent-usage 三个端点。
//
// ⚠️ 两个标识必须严格分开，否则桩测不出真实行为：
//   - 账号维度：Authorization 里的 **Cloud-IDE-JWT**（UgHeaders 唯一带的账号凭据）
//   - 设备维度：**x-device-id**
//
// 换号重试时 JWT 不变而 x-device-id 每次都不同 —— 这正是要模拟的场景。
// 若误用 x-device-id 当账号键，claim 与 ent-usage 会落在不同键上，
// 额度永远读成 0，测试表现为「换号链路跑通了但没加分」。
func (f *rotateFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	dev := r.Header.Get("x-device-id")
	uid := strings.TrimPrefix(r.Header.Get("Authorization"), "Cloud-IDE-JWT ")

	f.mu.Lock()
	defer f.mu.Unlock()
	f.uidSeq = append(f.uidSeq, uid)
	f.deviceSeq = append(f.deviceSeq, dev)

	reply := func(code int, msg string, data map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		raw, _ := json.Marshal(map[string]any{"code": code, "message": msg, "data": data})
		_, _ = w.Write(raw)
	}

	switch {
	case strings.HasSuffix(r.URL.Path, "checkin_credits/status"):
		checked := f.claimedAccounts[uid] || f.usedDevices[dev]
		reply(0, "", map[string]any{
			"checked_in": checked,
			"credits":    100,
			"enable":     true,
		})

	case strings.HasSuffix(r.URL.Path, "checkin_credits/claim"):
		f.claimHits++
		f.claimDevices = append(f.claimDevices, dev)
		// 模拟瞬时限流：前 N 次一律 9074
		if f.claimHits <= f.throttleFirstN {
			reply(CheckinRateLimitCode, "当前参与用户太多，请稍后再试", nil)
			return
		}
		// 设备维度判定：该设备号今天已被用过 → 9095
		if f.usedDevices[dev] {
			reply(CheckinAlreadyClaimedCode, "当前设备今日已经签到，请明日再来哦～", nil)
			return
		}
		// alwaysZero：返回成功但不加分（今天确实已领过的形态）
		if f.alwaysZero {
			reply(0, "ok", map[string]any{"success": true})
			return
		}
		// 账号维度判定：真正领到
		f.usedDevices[dev] = true
		f.claimedAccounts[uid] = true
		f.remain[uid] += 100
		reply(0, "ok", map[string]any{"success": true})

	case strings.HasSuffix(r.URL.Path, "web_user_ent_usage"):
		// ⚠️ 两个容易踩的形状坑：
		//  1) traework 的 doJSON **不剥信封**，原样返回整个 body；
		//     而 fetchEntUsage 直接把它当 data 解析 → 所以
		//     user_entitlement_pack_list 必须在**顶层**，不能裹在 data 里。
		//  2) 余额 = quota.credits_limit - usage.credits_amount（packRemain 的算法）；
		//     available_endpoint=0 才计入 usable()。
		w.Header().Set("Content-Type", "application/json")
		raw, _ := json.Marshal(map[string]any{
			"code": 0,
			"user_entitlement_pack_list": []map[string]any{{
				"entitlement_base_info": map[string]any{
					"available_endpoint": 0,
					"product_id":         208,
					"quota":              map[string]any{"credits_limit": float64(f.remain[uid])},
					"entitlement_id":     "test",
				},
				"usage":        map[string]any{"credits_amount": float64(0)},
				"display_desc": "测试包",
				"group_name":   "test",
			}},
		})
		_, _ = w.Write(raw)
		return

	default:
		reply(404, "not found", nil)
	}
}

// newRotateClient 构造指向假上游的 Client。
func newRotateClient(t *testing.T, f *rotateFixture, uid, device string) (*Client, *auth.Auth) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	c := New()
	c.UgHost = srv.URL
	c.CheckinRetryDelay = 0 // 测试里不等

	a := &auth.Auth{
		Kind: "traework", UID: uid,
		// AccessToken 必须等于 uid：本桩把 Authorization: Cloud-IDE-JWT 当账号标识
		//（UgHeaders 不发 X-User-Id，这是它唯一带的账号凭据）。
		AccessToken: uid, RefreshToken: "rt",
		DeviceID:  device,
		ExpiresAt: time.Now().Add(24 * time.Hour).Unix(),
	}
	return c, a
}

// randDeviceID 必须产出 16 位纯数字 —— 上游只校验这个格式。
func TestRandDeviceIDShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		d := randDeviceID()
		if len(d) != 16 {
			t.Fatalf("长度=%d want 16（值 %q）", len(d), d)
		}
		for _, c := range d {
			if c < '0' || c > '9' {
				t.Fatalf("含非数字字符 %q：%s", c, d)
			}
		}
		seen[d] = true
	}
	// 随机性：50 次不应全同（否则说明退化成常量）
	if len(seen) < 40 {
		t.Fatalf("随机性不足：50 次只产生 %d 个不同值", len(seen))
	}
}

// ★ 核心场景：9095（设备已签）→ 换号 → 成功入账。
//
// 这正是 2026-10-12 事故的形态：账号 A 先签用掉了设备号，
// 账号 B 当天本该领额度却被连带拒绝。
func TestClaimClaimRotatesOn9095AndCredits(t *testing.T) {
	f := newRotateFixture()
	// 模拟账号 A 已在**同一设备号**上签过
	f.usedDevices["44842564aaaa"] = true
	f.claimedAccounts["uid-A"] = true

	c, a := newRotateClient(t, f, "uid-B", "44842564aaaa")

	before := f.remain["uid-B"]
	if err := c.CheckinClaim(a); err != nil {
		t.Fatalf("换号后应成功：%v", err)
	}
	if got := f.remain["uid-B"]; got != before+100 {
		t.Fatalf("额度未增加：%d -> %d（期望 +100）", before, got)
	}
	// 原设备号不得被写入凭据（换号只作用于本次请求）
	if a.DeviceID != "44842564aaaa" {
		t.Fatalf("凭据设备号被污染：%q", a.DeviceID)
	}
	// 必须真的换过号：只看 claim 端点用过的设备号
	//（deviceSeq 混入了前置/后置的 ent-usage 查询，不能直接按下标取）
	if len(f.claimDevices) < 2 {
		t.Fatalf("未见换号重试，claim 设备号序列=%v", f.claimDevices)
	}
	if f.claimDevices[0] != "44842564aaaa" {
		t.Fatalf("首次 claim 应使用原设备号，实际 %q", f.claimDevices[0])
	}
	if f.claimDevices[1] == "44842564aaaa" {
		t.Fatal("换号后仍用原设备号，等于没换")
	}
}

// code==0 但额度没变 = 今天确实已领（假成功陷阱）。
//
// 这是最容易写错的一处：若以 code==0 判成功，
// 上层会显示「刚领到 100 分」，而实际一分没加。
func TestRotateStopsWhenCreditUnchanged(t *testing.T) {
	f := newRotateFixture()
	// 该账号今天已领过（账号维度），且设备号都算已用
	f.claimedAccounts["uid-A"] = true
	// alwaysZero：模拟「今天确实已经领过」——换任何新号都返回 code=0，
	// 但额度一分不涨。这是 code==0 假成功的真实形态。
	f.alwaysZero = true
	c, a := newRotateClient(t, f, "uid-A", "useddevice01")

	before := f.remain["uid-A"]
	err := c.CheckinClaim(a)
	if err == nil {
		t.Fatal("额度未增加时应返回错误（否则会被误报成已领到）")
	}
	if !strings.Contains(err.Error(), "already claimed") {
		t.Fatalf("错误语义不对：%v", err)
	}
	if got := f.remain["uid-A"]; got != before {
		t.Fatalf("额度不应变化：%d -> %d", before, got)
	}
}

// ★ 换号过程撞上 9074（瞬时限流）必须继续，不能放弃。
//
// v0.6.9 实测踩过：第 1 次换号撞上 9074 就放弃，
// 而同一天手工重试随机号连续多次都成功。
func TestRotateContinuesThrough9074(t *testing.T) {
	f := newRotateFixture()
	f.usedDevices["origdevice1"] = true
	f.throttleFirstN = 3 // 前 3 次请求全部 9074

	c, a := newRotateClient(t, f, "uid-C", "origdevice1")

	before := f.remain["uid-C"]
	if err := c.CheckinClaim(a); err != nil {
		t.Fatalf("连续 9074 后仍应换号成功：%v", err)
	}
	if got := f.remain["uid-C"]; got != before+100 {
		t.Fatalf("额度未增加：%d -> %d", before, got)
	}
	// 至少 4 次：原设备号 + 3 次 9074（每次都换新号）
	if f.claimHits < 4 {
		t.Fatalf("重试次数不足：claimHits=%d（应 ≥4）", f.claimHits)
	}
	// claimDevices[0] 是原设备号那一次（返回 9074），
	// [1] 起才是 claimWithRotatedDevice 内部的各次换号尝试 ——
	// 后者必须**每次都不同**，否则就是在原地重试而非换号。
	if len(f.claimDevices) < 2 {
		t.Fatalf("未见换号重试：%v", f.claimDevices)
	}
	for i := 2; i < len(f.claimDevices); i++ {
		if f.claimDevices[i] == f.claimDevices[i-1] {
			t.Fatalf("第 %d 次换号未用新设备号：%v", i-1, f.claimDevices)
		}
	}
	// 且换号序列里不应再出现原设备号
	for i := 1; i < len(f.claimDevices); i++ {
		if f.claimDevices[i] == "origdevice1" {
			t.Fatalf("换号后又用回原设备号：%v", f.claimDevices)
		}
	}
}

// 重试次数有上限，不能无限打上游。
func TestRotateGivesUpAfterMaxAttempts(t *testing.T) {
	f := newRotateFixture()
	f.usedDevices["origdevice1"] = true
	f.throttleFirstN = 1 << 20 // 永远 9074

	c, a := newRotateClient(t, f, "uid-D", "origdevice1")

	if err := c.CheckinClaim(a); err == nil {
		t.Fatal("持续限流时应返回错误")
	}
	// 1 次原设备号 + maxRotateAttempts 次换号
	if f.claimHits > maxRotateAttempts+2 {
		t.Fatalf("尝试次数超上限：claimHits=%d maxRotate=%d", f.claimHits, maxRotateAttempts)
	}
	if a.DeviceID != "origdevice1" {
		t.Fatalf("失败路径也必须还原凭据设备号，得到 %q", a.DeviceID)
	}
}

// 9074 的换号重试路径（原设备号直接限流时也要能换号成功）。
func TestRotateOn9074ThenSucceed(t *testing.T) {
	f := newRotateFixture()
	f.throttleFirstN = 2

	c, a := newRotateClient(t, f, "uid-E", "someolddevice")

	before := f.remain["uid-E"]
	if err := c.CheckinClaim(a); err != nil {
		t.Fatalf("9074 后换号应成功：%v", err)
	}
	if got := f.remain["uid-E"]; got != before+100 {
		t.Fatalf("额度未增加：%d -> %d", before, got)
	}
}

// 顺带确认一次成功路径**不会**多余地换号（避免无谓消耗额度/触发风控）。
func TestNoRotationWhenFirstClaimSucceeds(t *testing.T) {
	f := newRotateFixture()
	c, a := newRotateClient(t, f, "uid-F", "brandnewdev1")

	if err := c.CheckinClaim(a); err != nil {
		t.Fatalf("首次即应成功：%v", err)
	}
	if f.claimHits != 1 {
		t.Fatalf("不应有多余重试：claimHits=%d", f.claimHits)
	}
	if f.remain["uid-F"] != 100 {
		t.Fatalf("额度=%d want 100", f.remain["uid-F"])
	}
}

// 常量口径锁定（防止后人改错码值）。
func TestCheckinCodeConstants(t *testing.T) {
	if CheckinRateLimitCode != 9074 {
		t.Fatalf("9074 码值被改动：%d", CheckinRateLimitCode)
	}
	if CheckinAlreadyClaimedCode != 9095 {
		t.Fatalf("9095 码值被改动：%d", CheckinAlreadyClaimedCode)
	}
	if maxRotateAttempts != 8 {
		t.Fatalf("重试上限被改动：%d", maxRotateAttempts)
	}
}

// shortDevice 日志脱敏：不得输出完整设备号。
func TestShortDeviceTruncates(t *testing.T) {
	if got := shortDevice("44842564aabbccdd"); got != "448425…" {
		t.Fatalf("截断结果=%q", got)
	}
	if got := shortDevice("abc"); got != "abc" {
		t.Fatalf("短值应原样返回，得到 %q", got)
	}
	// 日志里不得出现完整设备号
	full := "44842564aabbccdd"
	if strings.Contains(shortDevice(full), full) {
		t.Fatal("日志不得包含完整设备号")
	}
	_ = strconv.Itoa(0) // 保持 strconv 导入被使用（若后续断言需要）
}
