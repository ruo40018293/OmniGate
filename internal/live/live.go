// Package live 维护「正在飞的请求」实时状态，供面板查询（R56）。
//
// 存在理由（2026-10-23 事故）：qodercn 排队时上游连上、只回保活碎片、约 180s 后
// 零内容断开。网关在保活重投期间**不向客户端发任何字节**，客户端界面只有计时器；
// 而唯一的痕迹是等待结束才写下的一行 app.log —— 用户看到的现象是「一直在计时，
// 没反应，只能挨个重启再试」。R56 把「等待期间的进度」变成可查询的状态：
// 每一次 /v1/chat/completions 从进入到结束都在这里登记一行，面板实时轮询即可看到
// 「当前在排队 / 第几跳重投 / 已等多久 / 上游说的前方多少人」。
//
// 设计约束：
//   - 只读快照，不参与调度判定（不因快照改变重试与挑号行为）；
//   - 有界：最多 maxLive 行，超限丢最旧，内存占用恒定；
//   - 无锁外调用：快照在锁内拷贝，调用方拿到的永远是稳定副本。
package live

import (
	"sort"
	"sync"
	"time"
)

// maxLive 同时在册的请求上限（正常并发远小于此值，超限丢最旧）。
const maxLive = 64

// maxNotes 单请求保留的进度事件条数（只留最近的，防长跑请求撑爆内存）。
const maxNotes = 12

// Note 一条进度事件（唯一原子 = 一次状态跃迁）。
type Note struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

// Entry 一个在飞请求的实时状态。
type Entry struct {
	ID        string    `json:"id"`
	Started   time.Time `json:"started"`
	Channel   string    `json:"channel"`
	Model     string    `json:"model"`
	Stream    bool      `json:"stream"`
	Phase     string    `json:"phase"` // 见下方 phase* 常量
	Attempts  int       `json:"attempts"`
	QueuePos  int       `json:"queue_pos,omitempty"`
	QueueWait int       `json:"queue_wait,omitempty"`
	Notes     []Note    `json:"notes,omitempty"`
	Updated   time.Time `json:"updated"`
	// ContentAt 上游第一个**内容**字节到达时刻（零值＝还没有内容）。
	//
	// R56b：受理帧让 TTFB 变得没有信息量（连接一建立就有字节），用户真正想知道的是
	// 「多久能看到第一个字」。两者分开记，面板才能把「在等上游」和「在出内容」讲清楚。
	ContentAt time.Time `json:"content_at,omitempty"`
}

// 请求阶段（面板按此显示中文）。
const (
	PhaseQueued    = "queued"   // 上游排队中（有明确前方人数/预计等待）
	PhaseRetrying  = "retrying" // 保活重投中（无明确队列信息，例如上游零内容断开）
	PhaseStreaming = "streaming"
	PhaseDone      = "done"
)

// Tracker 在飞请求登记表。
type Tracker struct {
	mu      sync.Mutex
	seq     int64
	entries map[string]*Entry
	order   []string // 插入序，用于超限淘汰最旧
	now     func() time.Time
}

// New 创建登记表。
func New() *Tracker {
	return &Tracker{entries: make(map[string]*Entry), now: time.Now}
}

// Begin 登记一个新请求，返回其 ID（用于后续 Update/End）。
func (t *Tracker) Begin(channel, model string, stream bool) string {
	if t == nil {
		return ""
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	id := now.Format("150405.000") + "-" + itoa(t.seq)
	t.entries[id] = &Entry{
		ID:      id,
		Started: now,
		Channel: channel,
		Model:   model,
		Stream:  stream,
		Phase:   PhaseQueued,
		Updated: now,
	}
	t.order = append(t.order, id)
	if len(t.order) > maxLive {
		drop := t.order[0]
		t.order = t.order[1:]
		delete(t.entries, drop)
	}
	return id
}

// SetPhase 更新阶段与队列信息（queuePos/queueWait 为 0 表示上游没给）。
func (t *Tracker) SetPhase(id, phase string, queuePos, queueWait int) {
	if t == nil || id == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[id]
	if !ok {
		return
	}
	e.Phase = phase
	if queuePos > 0 {
		e.QueuePos = queuePos
	}
	if queueWait > 0 {
		e.QueueWait = queueWait
	}
	e.Updated = t.now()
}

// Note 追加一条进度事件（例如「第 2 跳重投，已用 1m1s」「上游零内容断开」）。
func (t *Tracker) Note(id, text string) {
	if t == nil || id == "" || text == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[id]
	if !ok {
		return
	}
	now := t.now()
	e.Attempts++
	e.Notes = append(e.Notes, Note{At: now, Text: text})
	if len(e.Notes) > maxNotes {
		e.Notes = e.Notes[len(e.Notes)-maxNotes:]
	}
	e.Updated = now
}

// End 结束登记（从在飞表移除；历史留在 stats 请求记录里）。
// MarkContent 记录上游第一个内容字节到达（幂等：只记第一次）。
//
// 语义边界：只应由**上游真实内容**触发，不能由受理帧/注释帧触发，否则「等首字」
// 会退化成「等连接」——那正是 R56b 要修掉的观测盲区。
func (t *Tracker) MarkContent(id string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entries[id]
	if e == nil || !e.ContentAt.IsZero() {
		return
	}
	e.ContentAt = t.now()
	e.Updated = e.ContentAt
}

func (t *Tracker) End(id string) {
	if t == nil || id == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, id)
	for i, v := range t.order {
		if v == id {
			t.order = append(t.order[:i], t.order[i+1:]...)
			break
		}
	}
}

// Snapshot 返回当前在飞请求快照（按开始时间升序）。
func (t *Tracker) Snapshot() []Entry {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Entry, 0, len(t.entries))
	for _, e := range t.entries {
		cp := *e
		if len(e.Notes) > 0 {
			cp.Notes = append([]Note(nil), e.Notes...)
		}
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// itoa 小整数转字符串（避免为一行日志引入 strconv 依赖噪声）。
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
