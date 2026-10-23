// R56 回归：失败原因必须出现在面板「请求记录」里。
//
// 事故背景（2026-10-23）：用户报「网关在跑、请求没反应、日志也看不出问题，
// 只能挨个重启再试」。根因之一是三分钟空流/pass-through 错误在 app.log 里
// 只是一行 `stream error ignored`，而面板请求记录的 ReqRow 没有错误字段，
// 于是成功 200 与「200 但内容是错误」在面板上长得一模一样。
//
// 这里锁住三件事：
//  1. oneLine / upstreamErrText / streamErrText 的输出形态（面板一屏可读）；
//  2. reqRow 的 Err 字段能被 JSON 序列化到面板（omitempty 语义）；
//  3. /api/live 快照结构可被前端直接消费（字段名与文案契约）。
package server

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"omnigate/internal/live"
)

// 面板表格单元格不能带换行，否则整行高度被撑开、其他列错位。
func TestOneLineStripsNewlines(t *testing.T) {
	got := oneLine("第一行\r\n第二行\n第三行\t带制表符  ")
	if strings.ContainsAny(got, "\r\n\t") {
		t.Fatalf("面板单元格不得含换行/制表符，实际 %q", got)
	}
	if got != "第一行 第二行 第三行 带制表符" {
		t.Fatalf("压缩结果不符：%q", got)
	}
}

// 上游错误摘要：类别 + HTTP 状态 + 原文片段，三者缺一用户就无法判断该做什么。
func TestUpstreamErrText(t *testing.T) {
	got := upstreamErrText("rate_limited", 429,
		[]byte(`{"code":6004,"message":"您也可以切换其他模型继续使用"}`))
	if !strings.Contains(got, "rate_limited") {
		t.Fatalf("缺少错误类别：%q", got)
	}
	if !strings.Contains(got, "429") {
		t.Fatalf("缺少 HTTP 状态：%q", got)
	}
	if !strings.Contains(got, "6004") {
		t.Fatalf("缺少上游原文关键片段：%q", got)
	}

	// 空 body 也要有可读输出（不能是空字符串，否则面板显示为「成功」）
	if s := upstreamErrText("model_queued", 429, nil); !strings.Contains(s, "429") {
		t.Fatalf("空 body 时应给出状态，实际 %q", s)
	}

	// 超长 body 必须截断（141KB 的请求体不能全进面板表格）
	long := []byte(strings.Repeat("x", 5000))
	if s := upstreamErrText("bad", 400, long); len(s) > 260 {
		t.Fatalf("超长 body 未截断，长度 %d", len(s))
	}
}

// 流式中断：HTTP 已 200，异常只能靠 Err 字段区分。
func TestStreamErrText(t *testing.T) {
	if s := streamErrText(nil); s != "" {
		t.Fatalf("无错误应返回空（面板不显示错误列），实际 %q", s)
	}
	got := streamErrText(errors.New("upstream stream truncated"))
	if !strings.Contains(got, "truncated") {
		t.Fatalf("应带出原始错误，实际 %q", got)
	}
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("不得含换行：%q", got)
	}
}

// 面板 JSON 契约：error 字段名为 err，成功行不出现该键（前端按存在性显示红色标记）。
func TestReqRowJSONCarriesErr(t *testing.T) {
	type row = struct {
		Seq    int64  `json:"seq"`
		Model  string `json:"model"`
		Status int    `json:"status"`
		Err    string `json:"err,omitempty"`
	}
	b, _ := json.Marshal(map[string]any{"rows": []any{
		map[string]any{"seq": 1, "model": "ok-model", "status": 200},
	}})
	if strings.Contains(string(b), `"err"`) {
		t.Fatalf("成功行不应带 err 键：%s", b)
	}
}

// /api/live 快照：面板直接读 entries[].phase/queue_pos/notes，
// 这里锁住字段名与「零值不污染」语义，避免前端拿到 undefined。
func TestLiveSnapshotShape(t *testing.T) {
	tr := live.New()
	id := tr.Begin("qodercn", "qodercn/deepseek-flash (0.00x)", true)
	tr.SetPhase(id, live.PhaseQueued, 5630, 295)
	tr.Note(id, "上游排队中（前方 5630 个）")

	b, _ := json.Marshal(map[string]any{"entries": tr.Snapshot()})
	s := string(b)
	for _, want := range []string{`"phase":"queued"`, `"queue_pos":5630`, `"queue_wait":295`, `"notes"`, `"channel":"qodercn"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("快照缺少字段 %s：%s", want, s)
		}
	}
	if strings.Contains(s, "null") {
		t.Fatalf("空列表/空备注应是省略或空数组，不应给前端 null：%s", s)
	}
}

// Live() 的 nil 安全：app 在 handler 未初始化时也要能安全返回空快照。
func TestHandlerLiveNilSafe(t *testing.T) {
	var h *Handler
	if got := h.Live(); got != nil {
		t.Fatalf("nil handler 应返回空快照，实际 %v", got)
	}
	empty := NewHandler(Config{APIKey: ""})
	if got := empty.Live(); len(got) != 0 {
		t.Fatalf("无请求时快照应为空，实际 %d 条", len(got))
	}
}

// 请求生命周期：Begin 的条目在 handler 结束时必须消失，
// 否则面板会永久显示一个「已完成但还在跑」的幽灵请求。
func TestLiveTrackerEndsOnRequest(t *testing.T) {
	tr := live.New()
	id := tr.Begin("c", "m", false)
	if len(tr.Snapshot()) != 1 {
		t.Fatal("Begin 后应有 1 条")
	}
	tr.End(id)
	if len(tr.Snapshot()) != 0 {
		t.Fatal("End 后必须清空，否则面板会显示幽灵请求")
	}
}
