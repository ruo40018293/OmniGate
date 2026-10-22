package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSaveOnError 异常时必须落盘，且内容完整可读（含请求体与回包）。
func TestSaveOnError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvDir, dir)
	t.Setenv(EnvOff, "")
	ResetDirCache()

	path := Save(Record{
		Channel:  "workbuddyai",
		UID:      "e0f0b910-c011",
		Model:    "deepseek-v4.1-flash",
		Status:   403,
		Classify: "content_blocked",
		Request:  []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"IDENTITY_SENTINEL"}]}`),
		Response: []byte(`{"code":"11140","displayMsg":{"zh":"账号存在异常行为"}}`),
	})
	if path == "" {
		t.Fatal("异常记录未落盘：Save 返回空路径")
	}
	if !strings.HasSuffix(path, ".json") {
		t.Fatalf("文件名不是 .json：%s", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回落盘文件失败：%v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("落盘内容不是合法 JSON：%v", err)
	}
	if got["channel"] != "workbuddyai" {
		t.Errorf("channel=%v，期望 workbuddyai", got["channel"])
	}
	if got["uid"] != "e0f0b910-c011" {
		t.Errorf("uid=%v 丢了（无法定位是哪个账号）", got["uid"])
	}
	if got["classify"] != "content_blocked" {
		t.Errorf("classify=%v，期望 content_blocked", got["classify"])
	}
	// 关键断言：请求体原文必须可 grep（这是做二分的唯一依据）
	if req, _ := got["request"].(string); !strings.Contains(req, "IDENTITY_SENTINEL") {
		t.Errorf("落盘请求体丢了原文，无法二分：%q", req)
	}
	if resp, _ := got["response"].(string); !strings.Contains(resp, "11140") {
		t.Errorf("落盘回包丢了原文：%q", resp)
	}
	// 隐私边界：不得写任何请求头/token
	for _, leak := range []string{"Authorization", "Bearer ", "Cookie", "accessToken"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("落盘内容泄露敏感字段 %q", leak)
		}
	}
}

// TestSaveTransportError 传输层失败（status=0）也要留证。
func TestSaveTransportError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvDir, dir)
	t.Setenv(EnvOff, "")
	ResetDirCache()

	path := Save(Record{
		Channel: "workbuddy",
		UID:     "196316fc",
		Model:   "deepseek-v4.1-flash",
		Err:     "read tcp: connection reset by peer",
		Request: []byte(`{"model":"deepseek-v4.1-flash"}`),
	})
	if path == "" {
		t.Fatal("传输层失败未落盘")
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "connection reset") {
		t.Error("落盘内容没记下传输层错误原文")
	}
	if !strings.Contains(string(raw), "196316fc") {
		t.Error("落盘内容没记下账号 uid")
	}
}

// TestSaveDisabled 显式关闭时必须零写盘。
func TestSaveDisabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvDir, dir)
	t.Setenv(EnvOff, "1")
	ResetDirCache()

	if p := Save(Record{Channel: "x", Status: 500, Request: []byte("{}")}); p != "" {
		t.Fatalf("关闭状态下仍然落盘：%s", p)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Fatalf("关闭状态下目录里有 %d 个文件，期望 0", len(ents))
	}
}

// TestResponseTruncated 超大回包必须被截断（防错误页爆盘）。
func TestResponseTruncated(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvDir, dir)
	t.Setenv(EnvOff, "")
	ResetDirCache()

	big := strings.Repeat("x", maxBodyBytes+5000)
	path := Save(Record{Channel: "y", Status: 502, Request: []byte("{}"), Response: []byte(big)})
	if path == "" {
		t.Fatal("未落盘")
	}
	raw, _ := os.ReadFile(path)
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	if trunc, _ := got["response_truncated"].(bool); !trunc {
		t.Error("超大回包未标记 truncated")
	}
	if resp, _ := got["response"].(string); len(resp) > maxBodyBytes {
		t.Errorf("回包未截断：%d > %d", len(resp), maxBodyBytes)
	}
}

// TestFileNamesUnique 同一纳秒内的多次异常不能互相覆盖。
func TestFileNamesUnique(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvDir, dir)
	t.Setenv(EnvOff, "")
	ResetDirCache()

	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		p := Save(Record{Channel: "z", Status: 400, Request: []byte(strings.Repeat("a", i+1))})
		if p == "" {
			t.Fatal("未落盘")
		}
		base := filepath.Base(p)
		if seen[base] {
			t.Fatalf("文件名重复，会互相覆盖：%s", base)
		}
		seen[base] = true
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 20 {
		t.Fatalf("目录内 %d 个文件，期望 20（有覆盖发生）", len(ents))
	}
}

// TestModelFromBody 取模型名容错。
func TestModelFromBody(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`{"model":"deepseek-v4.1-flash"}`, "deepseek-v4.1-flash"},
		{`{"messages":[]}`, ""},
		{`not json`, ""},
		{``, ""},
	}
	for _, c := range cases {
		if got := ModelFromBody([]byte(c.in)); got != c.want {
			t.Errorf("ModelFromBody(%q)=%q，期望 %q", c.in, got, c.want)
		}
	}
}
