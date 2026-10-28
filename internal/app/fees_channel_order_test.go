package app

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestFeeChannelOrderCoversEveryKind 守住 R65 的坑：新增渠道若只接了 runtime、
// 没登记进 feeChannelOrder，「模型列表和费率」面板整组不出现（buildFeesChannels
// 按该固定顺序遍历），用户看到的是「面板里没模型」，而 /v1/models 其实有。
//
// R63 接 MiniMax/Cline 时正是漏了这一步；这条测试用「源码扫描」把漏项变成编译期
// 之外的显式失败——Go 无法在运行时枚举常量，所以扫 provider 包的常量声明。
func TestFeeChannelOrderCoversEveryKind(t *testing.T) {
	// 已从界面下线的渠道：不展示、不拉费率（见 buildFeesChannels 的 Qoder 分支）。
	exempt := map[string]bool{"Qoder": true}

	providerSrc := readRepoFile(t, "internal/provider/provider.go")
	kindDecl := regexp.MustCompile(`(?m)^\s*([A-Z]\w*)\s+Kind\s*=\s*"`) // Kind 常量声明
	declared := map[string]bool{}
	for _, m := range kindDecl.FindAllStringSubmatch(providerSrc, -1) {
		declared[m[1]] = true
	}
	if len(declared) < 10 {
		t.Fatalf("从 provider.go 只解析出 %d 个 Kind 常量，解析逻辑已失效（不要放宽这个下限）", len(declared))
	}

	appSrc := readRepoFile(t, "internal/app/app.go")
	block := extractVarBlock(t, appSrc, "feeChannelOrder")
	for name := range declared {
		if exempt[name] {
			continue
		}
		if !strings.Contains(block, "provider."+name) {
			t.Errorf("渠道 %s 未登记到 feeChannelOrder：面板「模型列表和费率」会整组不显示（R65）", name)
		}
	}

	// R65 新增渠道必须显式在列（防止有人把上面整段改成通配实现后静默回退）。
	for _, name := range []string{"provider.Minimax", "provider.Cline"} {
		if !strings.Contains(block, name) {
			t.Errorf("feeChannelOrder 缺少 %s（R65）", name)
		}
	}
}

// TestFeeChannelOrderNoDuplicate 重复登记会让同一渠道在面板里出现两个 tab。
func TestFeeChannelOrderNoDuplicate(t *testing.T) {
	appSrc := readRepoFile(t, "internal/app/app.go")
	block := extractVarBlock(t, appSrc, "feeChannelOrder")
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`provider\.(\w+)`).FindAllStringSubmatch(block, -1) {
		if seen[m[1]] {
			t.Errorf("feeChannelOrder 重复登记 provider.%s", m[1])
		}
		seen[m[1]] = true
	}
	if len(seen) == 0 {
		t.Fatal("feeChannelOrder 解析为空，测试失效")
	}
}

// readRepoFile 读取仓库内文件：从测试文件所在包目录向上找到 go.mod。
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(dir + "/go.mod"); err == nil {
			b, err := os.ReadFile(dir + "/" + rel)
			if err != nil {
				t.Fatalf("读取 %s: %v", rel, err)
			}
			return string(b)
		}
		dir = dir + "/.."
	}
	t.Fatalf("向上 6 层仍未找到 go.mod，无法定位仓库根")
	return ""
}

// extractVarBlock 取出 `var <name> = ...` / `var <name> <type> = ...` 的花括号块。
func extractVarBlock(t *testing.T, src, name string) string {
	t.Helper()
	idx := strings.Index(src, "var "+name)
	if idx < 0 {
		t.Fatalf("源码里找不到 var %s", name)
	}
	start := strings.Index(src[idx:], "{")
	if start < 0 {
		t.Fatalf("var %s 后没有花括号块", name)
	}
	start += idx
	depth := 0
	for i := start; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1]
			}
		}
	}
	t.Fatalf("var %s 的花括号未闭合", name)
	return ""
}
