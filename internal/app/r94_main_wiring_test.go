package app

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// R94 接线闸门：签到时刻的「跟随全局」白名单必须与渠道的真实窗口语义一致。
//
// 为什么用源码级闸门而不是行为测试：这条约束的表达位置就是装配代码
// （cmd/omnigate/main.go 的 scheduler.Config 字面量）。走行为测试需要把
// main() 的整套装配搬进测试，代价远大于收益；而这条约束一旦被误改
// （比如给 qodercn 加上 FollowGlobalCheckin），后果是当天静默漏领，
// 单元测试也不会红——正是必须用闸门锁住的类型。
func TestMainFollowsGlobalCheckinWhitelist(t *testing.T) {
	path := filepath.Join("..", "..", "cmd", "omnigate", "main.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	src := string(raw)

	const marker = "scheduler.New(scheduler.Config{"
	parts := strings.Split(src, marker)
	if len(parts) < 2 {
		t.Fatalf("main.go 里没有找到任何 %s 字面量", marker)
	}

	nameRe := regexp.MustCompile(`Name: "([a-z]+)"`)
	got := map[string]bool{}
	for _, part := range parts[1:] {
		end := strings.Index(part, "})")
		if end < 0 {
			t.Fatalf("无法定位 scheduler.Config 字面量的结尾：%.120s", part)
		}
		chunk := part[:end]
		m := nameRe.FindStringSubmatch(chunk)
		if m == nil {
			t.Fatalf("scheduler.Config 字面量缺少 Name：%.120s", chunk)
		}
		got[m[1]] = strings.Contains(chunk, "FollowGlobalCheckin:")
	}

	// 这些渠道的签到时刻就是面板上的全局时刻，必须跟随。
	// lobsterai（R104）：上游活动按自然日开放、没有固定窗口，跟随全局时刻。
	mustFollow := []string{"workbuddy", "traework", "workbuddyai", "raccoon", "glm", "lobsterai", "officeace", "codearts"}
	for _, name := range mustFollow {
		v, ok := got[name]
		if !ok {
			t.Errorf("找不到 %q 的 scheduler 装配", name)
			continue
		}
		if !v {
			t.Errorf("%q 的签到时刻来自全局设置，必须声明 FollowGlobalCheckin: true", name)
		}
	}

	// Qoder 双区固定在 10:00 开放、MiniMax 在 00:00 重置积分，
	// 一旦跟随面板时刻被覆盖，当天直接漏领；无签到渠道跟随只会每天空跑。
	mustNotFollow := []string{
		"qodercn", "qodercom", "minimax",
		"qoder", "qwenwork", "monkeycode", "loomy", "cline",
		// Gemini（R106）：没有签到机制（配额为窗口剩余百分比，DailyCheckin 返回明确的
		// 「本渠道没有签到机制」错误），跟随面板时刻只会每天空跑两次并污染面板。
		"gemini",
	}
	for _, name := range mustNotFollow {
		v, ok := got[name]
		if !ok {
			t.Errorf("找不到 %q 的 scheduler 装配", name)
			continue
		}
		if v {
			t.Errorf("%q 不得声明 FollowGlobalCheckin（固定窗口或无签到，覆盖即漏领或空跑）", name)
		}
	}

	if len(got) != len(mustFollow)+len(mustNotFollow) {
		t.Errorf("渠道数 %d 与白名单总数 %d 不符：%v", len(got), len(mustFollow)+len(mustNotFollow), got)
	}
}
