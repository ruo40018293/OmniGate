package scheduler

import (
	"testing"
	"time"

	"omnigate/internal/auth"
	"omnigate/internal/pool"
)

// R94：面板改签到时间时，只有声明跟随全局时刻的渠道会被推送新时刻。
//
// 这里锁住的是「默认不跟随」：固定窗口渠道（Qoder 双区 10:00 开放、
// MiniMax 00:00 重置积分）与无签到渠道靠的就是这个默认值——如果哪天
// 有人把 FollowsGlobalCheckin 的默认行为改成 true，那两个渠道的窗口会被
// 用户随便改的时间覆盖，当天直接漏领。
func TestFollowsGlobalCheckinDefaultsToFalse(t *testing.T) {
	s := New(Config{Pool: pool.New(""), Name: "qodercn", CheckinMinutes: []int{10 * 60}})
	if s.FollowsGlobalCheckin() {
		t.Fatal("未显式声明时必须默认不跟随全局签到时刻（固定窗口渠道依赖这个默认值）")
	}
}

func TestFollowsGlobalCheckinHonoursConfig(t *testing.T) {
	s := New(Config{Pool: pool.New(""), Name: "workbuddy", CheckinMinutes: []int{9 * 60}, FollowGlobalCheckin: true})
	if !s.FollowsGlobalCheckin() {
		t.Fatal("显式声明 FollowGlobalCheckin 后必须返回 true")
	}
}

// R94：本渠道没有签到任务（CheckinMinutes 显式空切片）时，签到必须既不落盘
// 也不上报。否则面板会把每天一次必然失败的调用显示成「签到失败」，用户以为
// 渠道坏了，而事实是这个渠道根本没有签到活动。
func TestFinishCheckinSkipsChannelsWithoutCheckin(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{Kind: "loomy", UID: "loomy-1", Nickname: "loomy", AccessToken: "t", RefreshToken: ""})
	s := New(Config{Pool: p, Name: "loomy", CheckinMinutes: []int{}})

	// 前置：确认构造后确实没有签到时刻（nil 会被 New 补成默认 9:00/21:00）。
	if got := s.CheckinMinutes(); len(got) != 0 {
		t.Fatalf("CheckinMinutes 应为空，实际 %v", got)
	}

	s.finishCheckin("loomy-1", CheckinResult{UID: "loomy-1", Msg: "no refresh token", Retryable: true})

	st, ok := p.Status("loomy-1")
	if !ok {
		t.Fatal("账号应存在于池中")
	}
	if !st.LastCheckinAt.IsZero() {
		t.Fatalf("无签到任务的渠道不得记录签到状态，实际 last_checkin_at=%s msg=%q", st.LastCheckinAt, st.LastCheckinMsg)
	}
}

// 有签到任务的渠道照常记录——这条是上一条的对照，防止把闸门写成「永不记录」。
func TestFinishCheckinRecordsChannelsWithCheckin(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{Kind: "workbuddy", UID: "wb-1", Nickname: "wb", AccessToken: "t", RefreshToken: "r"})
	s := New(Config{Pool: p, Name: "workbuddy", CheckinMinutes: []int{9 * 60}})

	before := time.Now()
	s.finishCheckin("wb-1", CheckinResult{UID: "wb-1", OK: true, Msg: "claimed"})

	st, _ := p.Status("wb-1")
	if st.LastCheckinAt.Before(before.Add(-time.Minute)) || st.LastCheckinAt.IsZero() {
		t.Fatalf("有签到任务的渠道必须记录 last_checkin_at，实际 %s", st.LastCheckinAt)
	}
	if !st.LastCheckinOK {
		t.Fatal("签到成功状态必须落盘")
	}
}
