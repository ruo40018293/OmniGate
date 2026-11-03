package qwenwork

import (
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// 回归（issue #42）：千问办公的流（envelope 包裹）被「可证实地」截断时，出口必须发
// error 帧且**不补** [DONE]，不能把半截流伪装成正常收尾。
//
// 修复前 streamAsOpenAI 的 sawDone 是死变量，于是连接中断/读超时被掐断与断在半个帧
// 上两种情况都会照常补 data: [DONE] → 客户端拿着半截 tool_call arguments 报
// "tool input was not fully received"，网关日志却无错误。
func TestStreamTruncationNotDisguisedAsDone(t *testing.T) {
	// envelope 写法：{body: "<inner json>"}，inner json 含 content 与一个未收尾的 tool_call。
	// 注意：envelope 的 body 字段是**转义后的字符串**，所以下面的 inner 必须自带反斜杠转义；
	// 直接写裸 JSON 会得到非法外层 JSON（本文件曾因此测出假阴性）。
	framef := func(inner string) string { return `data: {"body":"` + inner + `"}` + "\n\n" }
	content := `{\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}`
	toolPartial := `{\"id\": \"x\", \"choices\": [{\"index\": 0, \"delta\": {\"tool_calls\": [{\"index\": 0, \"function\": {\"name\": \"Bash\", \"arguments\": \"{\\\"cmd\\\"\"}}]}}]}`
	// toolCut：body 是**合法字符串**，但其内容 JSON 被切断（缺右花括号）。
	// 与 toolPartial 的区别：toolPartial 内层其实是合法 JSON（只是 arguments 半截），
	// toolCut 内层整体不合法 —— 才是 innerChunk 返回 nil 的真实场景。
	toolCut := `{\"id\":\"x\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"{\\\"cmd\\\"`
	dones := "data: {\"body\":\"[DONE]\"}\n\n"
	full := framef(content) + framef(toolPartial)

	cases := []struct {
		name       string
		rd         io.Reader
		wantErr    bool
		wantDone   bool
		wantErrFrm bool
	}{
		{"传输层读错误", &qwReadErr{payload: full}, true, false, true},
		{"断在半个帧上", &qwCut{payload: full, keep: len(full) - 10}, true, false, true},
		{"正常 [DONE] 收尾", strings.NewReader(full + dones), false, true, false},
		{"EOF 但上游漏发 [DONE]", strings.NewReader(full), false, true, false},
		// R90：这一条才是上一条真 bug 的靶心 —— 传输层**干净 EOF**（rd 不报错），
		// 但末帧内层是半截 JSON（解不出来）。innerChunk 对「解不出来」一律返回 nil，
		// 与 body=="[DONE]" 完全同形；没有 innerTruncated 就只能当正常收尾。
		{"干净 EOF 但末帧内层是半截 JSON", strings.NewReader(framef(toolCut)), true, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			_, err := Stream(rec, c.rd, "m")
			body := rec.Body.String()
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, c.wantErr)
			}
			if got := strings.Contains(body, "[DONE]"); got != c.wantDone {
				t.Errorf("[DONE]=%v want %v（截断不得伪装成正常收尾）\nbody=%s", got, c.wantDone, body)
			}
			if got := strings.Contains(body, "upstream_truncated"); got != c.wantErrFrm {
				t.Errorf("error frame=%v want %v\nbody=%s", got, c.wantErrFrm, body)
			}
		})
	}
}

type qwReadErr struct {
	payload string
	sent    bool
}

func (r *qwReadErr) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, r.payload), nil
	}
	return 0, fmt.Errorf("simulated: connection reset")
}

type qwCut struct {
	payload   string
	keep, off int
}

func (r *qwCut) Read(p []byte) (int, error) {
	if r.off >= r.keep {
		return 0, io.EOF
	}
	n := copy(p, r.payload[r.off:r.keep])
	r.off += n
	return n, nil
}
