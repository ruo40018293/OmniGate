package ledger

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// seedSplit 三笔已知输入/输出：合计 1460 = 输入 1300 + 输出 160。
func seedSplit(t *testing.T) *Ledger {
	t.Helper()
	l, err := New(filepath.Join(t.TempDir(), "ledger"))
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	l.AppendUsage(UsageEntry{Ch: "qodercn", UID: "u1", Model: "m1", PT: 900, CT: 100})
	l.AppendUsage(UsageEntry{Ch: "qwenwork", UID: "u2", Model: "m2", PT: 400, CT: 60})
	return l
}

// TestDayTokensSplitInOut 按天桶必须同时给出 in/out 与分渠道 in/out。
func TestDayTokensSplitInOut(t *testing.T) {
	l := seedSplit(t)
	defer l.Close()

	st := l.Query(7, nil)
	if len(st.Token.ByDay) == 0 {
		t.Fatal("by_day 为空")
	}
	dt := st.Token.ByDay[len(st.Token.ByDay)-1] // 最新一天（刚写入）
	if dt.Total != 1460 || dt.In != 1300 || dt.Out != 160 {
		t.Fatalf("day total/in/out = %d/%d/%d want 1460/1300/160", dt.Total, dt.In, dt.Out)
	}
	if dt.In+dt.Out != dt.Total {
		t.Fatalf("in+out=%d != total=%d", dt.In+dt.Out, dt.Total)
	}
	// 每个渠道：入+出 == 合计；键集与 by_channel 一致（缺键会让前端 NaN）
	if len(dt.ByChannelIn) != len(dt.ByChannel) || len(dt.ByChannelOut) != len(dt.ByChannel) {
		t.Fatalf("键集不一致：ch=%d in=%d out=%d", len(dt.ByChannel), len(dt.ByChannelIn), len(dt.ByChannelOut))
	}
	var sumIn, sumOut int64
	for ch, total := range dt.ByChannel {
		in, out := dt.ByChannelIn[ch], dt.ByChannelOut[ch]
		if in+out != total {
			t.Fatalf("渠道 %s：入%d+出%d != 合计%d", ch, in, out, total)
		}
		sumIn += in
		sumOut += out
	}
	if sumIn != dt.In || sumOut != dt.Out {
		t.Fatalf("渠道侧求和不等于日合计：入%d/%d 出%d/%d", sumIn, dt.In, sumOut, dt.Out)
	}
	// 具体值：qodercn 只有输入 900/输出 100
	if dt.ByChannelIn["qodercn"] != 900 || dt.ByChannelOut["qodercn"] != 100 {
		t.Fatalf("qodercn in/out = %d/%d want 900/100", dt.ByChannelIn["qodercn"], dt.ByChannelOut["qodercn"])
	}
}

// TestHourTokensSplitInOut days=1 的小时桶同样带拆分。
func TestHourTokensSplitInOut(t *testing.T) {
	l := seedSplit(t)
	defer l.Close()

	st := l.Query(1, nil)
	last := st.Token.ByDay[len(st.Token.ByDay)-1]
	if last.In != 1300 || last.Out != 160 {
		t.Fatalf("当前小时 in/out = %d/%d want 1300/160", last.In, last.Out)
	}
	// 空小时桶：不能 panic，且分母侧为空映射（前端按 key 取值不炸）
	for _, h := range st.Token.ByDay[:len(st.Token.ByDay)-1] {
		if h.Total != 0 || len(h.ByChannelIn) != 0 {
			t.Fatalf("空小时 %s 不该有数据：total=%d in=%d", h.Date, h.Total, len(h.ByChannelIn))
		}
	}
}

// TestDayTokensSplitJSON 序列化契约：老字段名不变，新字段为 in/out，
// 空桶的 by_channel_in/out 因 omitempty 不出现在 JSON 里（控制体积）。
func TestDayTokensSplitJSON(t *testing.T) {
	full, err := json.Marshal(DayTokens{
		Date: "10-29", Total: 1460, In: 1300, Out: 160,
		ByChannel:    map[string]int64{"qodercn": 1000},
		ByChannelIn:  map[string]int64{"qodercn": 900},
		ByChannelOut: map[string]int64{"qodercn": 100},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"in":1300`, `"out":160`, `"by_channel_in":`, `"by_channel_out":`} {
		if !strings.Contains(string(full), want) {
			t.Fatalf("JSON 缺 %s：%s", want, full)
		}
	}
	empty, _ := json.Marshal(DayTokens{Date: "00:00", ByChannel: map[string]int64{}})
	if strings.Contains(string(empty), "by_channel_in") {
		t.Fatalf("空桶不该带 by_channel_in：%s", empty)
	}
}
