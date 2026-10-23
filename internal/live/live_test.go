// R56 回归：在飞请求实时表（面板 /api/live 的数据源）。
//
// 事故背景：qodercn 排队时上游只回保活碎片、约 180s 后零内容断开；网关在保活
// 重投期间不向客户端发任何字节，客户端界面只剩一个计时器，用户体感是「卡住了，
// 只能挨个重启再试」。这里锁住三件事：
//  1. 一个请求从 Begin 到 End 的生命周期在表里可见；
//  2. 排队/重投阶段与队列信息能被更新（面板据此区分「在等」与「卡死」）；
//  3. 表有界（不因长跑/高并发无限增长），End 后立即消失。
package live

import (
	"sync"
	"testing"
	"time"
)

func TestTrackerLifecycle(t *testing.T) {
	tr := New()
	id := tr.Begin("qodercn", "qodercn/deepseek-flash (0.00x)", true)
	if id == "" {
		t.Fatal("Begin 应返回非空 ID，否则 handler 无法更新/注销该请求")
	}
	got := tr.Snapshot()
	if len(got) != 1 {
		t.Fatalf("快照应恰有 1 条在飞请求，实际 %d", len(got))
	}
	e := got[0]
	if e.Channel != "qodercn" || e.Stream != true {
		t.Fatalf("渠道/流式标记登记错误: %+v", e)
	}
	if e.Model != "qodercn/deepseek-flash (0.00x)" {
		t.Fatalf("模型名应保留客户端原始形态（面板要显示用户请求的），实际 %q", e.Model)
	}

	tr.SetPhase(id, PhaseQueued, 3058, 185)
	tr.Note(id, "上游排队中（前方 3058 个，预计 185s），进入保活重投")
	got = tr.Snapshot()
	if got[0].Phase != PhaseQueued || got[0].QueuePos != 3058 || got[0].QueueWait != 185 {
		t.Fatalf("排队信息未生效: %+v", got[0])
	}
	if got[0].Attempts != 1 || len(got[0].Notes) != 1 {
		t.Fatalf("事件计数错误: attempts=%d notes=%d", got[0].Attempts, len(got[0].Notes))
	}

	tr.End(id)
	if n := len(tr.Snapshot()); n != 0 {
		t.Fatalf("End 后应立即从在飞表消失，实际还有 %d 条", n)
	}
}

// 队列信息只在非零时覆盖：上游某些形态（零内容断开）不给前方人数，
// 若把 0 写进去会把上一跳的真实队列信息抹掉，面板就会显示「前方 0 个」。
func TestTrackerKeepsLastQueueInfoOnZero(t *testing.T) {
	tr := New()
	id := tr.Begin("qodercn", "m", true)
	tr.SetPhase(id, PhaseQueued, 5630, 295)
	tr.SetPhase(id, PhaseRetrying, 0, 0) // 零内容断开形态
	e := tr.Snapshot()[0]
	if e.Phase != PhaseRetrying {
		t.Fatalf("阶段应更新为 retrying，实际 %q", e.Phase)
	}
	if e.QueuePos != 5630 || e.QueueWait != 295 {
		t.Fatalf("上游未给队列信息时不得抹掉上一跳真实值，实际 pos=%d wait=%d",
			e.QueuePos, e.QueueWait)
	}
}

// 事件列表有界：一个长跑请求可以重投几十跳，全量保存会撑爆内存。
func TestTrackerNotesBounded(t *testing.T) {
	tr := New()
	id := tr.Begin("qodercn", "m", true)
	for i := 0; i < maxNotes*3; i++ {
		tr.Note(id, "重投")
	}
	e := tr.Snapshot()[0]
	if len(e.Notes) != maxNotes {
		t.Fatalf("事件列表应裁到 %d 条，实际 %d", maxNotes, len(e.Notes))
	}
	if e.Attempts != maxNotes*3 {
		t.Fatalf("Attempts 应累计全部事件（裁剪只影响明细），实际 %d", e.Attempts)
	}
}

// 在册表有界：超限淘汰最旧，且被淘汰的请求不能再更新（不能凭空复活）。
func TestTrackerBounded(t *testing.T) {
	tr := New()
	var first string
	for i := 0; i < maxLive+5; i++ {
		id := tr.Begin("c", "m", false)
		if i == 0 {
			first = id
		}
	}
	if n := len(tr.Snapshot()); n != maxLive {
		t.Fatalf("在飞表应封顶 %d 条，实际 %d", maxLive, n)
	}
	tr.SetPhase(first, PhaseQueued, 1, 1) // 已被淘汰，应为空操作
	for _, e := range tr.Snapshot() {
		if e.ID == first {
			t.Fatal("已淘汰的请求不应复活")
		}
	}
}

// 并发安全：handler 会在多个 goroutine 里同时 Begin/Note/End/Snapshot。
// 用 -race 跑能覆盖数据竞争；这里同时锁住「快照是副本」——
// 调用方改动快照不能影响内部状态（否则面板读取会污染真实状态）。
func TestTrackerConcurrentAndSnapshotIsCopy(t *testing.T) {
	tr := New()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := tr.Begin("c", "m", true)
			tr.Note(id, "x")
			tr.SetPhase(id, PhaseQueued, i+1, i+1)
			_ = tr.Snapshot()
			if i%2 == 0 {
				tr.End(id)
			}
		}(i)
	}
	wg.Wait()

	snap := tr.Snapshot()
	if len(snap) == 0 {
		t.Skip("并发下已全部结束，无快照可验证副本语义")
	}
	orig := snap[0].Channel
	snap[0].Channel = "被调用方改坏"
	if tr.Snapshot()[0].Channel != orig {
		t.Fatal("Snapshot 必须返回副本，调用方修改不得污染内部状态")
	}
}

// 时间注入：面板显示「已等多久」依赖 Started，锁住 Begin 用的是 tracker 的时钟。
func TestTrackerUsesInjectedClock(t *testing.T) {
	tr := New()
	fixed := time.Date(2026, 10, 23, 15, 4, 5, 0, time.UTC)
	tr.now = func() time.Time { return fixed }
	id := tr.Begin("c", "m", true)
	if e := tr.Snapshot()[0]; !e.Started.Equal(fixed) {
		t.Fatalf("Started 应来自 tracker 时钟，实际 %v", e.Started)
	}
	tr.Note(id, "x")
	if e := tr.Snapshot()[0]; !e.Updated.Equal(fixed) {
		t.Fatalf("Updated 应随事件刷新，实际 %v", e.Updated)
	}
}
