// identity_parity_test.go R51 跨包护栏：客户端身份串在四个位置各存一份
// （渠道包之间不共享代码是既有纪律 R44/R58，隔离是有意的，但**一致性**必须
// 由机器守住，不能靠人记）。
//
// 这条护栏的由来是上游同类事故（sddvcm/workbuddy-api 与 rockswang/wild-work
// 在 11-128 修复上都踩了"只改一份表等于没修"）：任何一处漏同步，都是"改完
// 看起来通过了、实际另一半链路还是旧身份"的静默失效。故这里用源码扫描而不是
// 反射——跨包常量彼此不可见，扫源码是唯一能在单测里发现"某处漏改"的手段。
package upstream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 身份链唯一真源（与 headers.go 的常量同值；改这里等于改四处，所以两者
// 一起被 TestIdentityHeadersMatchOfficialDesktop 锁着）。
const (
	wantIdentityUA   = "WorkBuddy/5.8.4 WorkBuddy/5.8.4 CLI/2.161.0"
	wantIdentityVer  = "5.8.4"
	wantIdentityProd = "SaaS"
)

// identitySites 所有必须持有同一身份串的源码位置（相对仓库根）。
// 新增渠道若沿用 workbuddy 协议，必须加进来。
var identitySites = []struct {
	path string
	kind string // ua | ver | prod
}{
	{"internal/upstream/headers.go", "ua"},
	{"internal/workbuddyai/constants.go", "ua"},
	{"internal/login/login.go", "ua"},
	{"internal/login_wbai/login.go", "ua"},
	{"internal/workbuddyai/constants.go", "ver"},
	{"internal/workbuddyai/constants.go", "prod"},
}

// TestIdentityStringParityEverywhere：所有站点的身份串必须一致。
// 把任一处改回旧值（例如 internal/login/login.go 里留 CLI/2.63.2），本测试变红。
func TestIdentityStringParityEverywhere(t *testing.T) {
	root := repoRoot(t)
	for _, site := range identitySites {
		p := filepath.Join(root, site.path)
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("读不到 %s：%v（改名后请同步 identitySites）", site.path, err)
		}
		src := string(raw)
		switch site.kind {
		case "ua":
			if !strings.Contains(src, wantIdentityUA) {
				t.Errorf("%s 缺少统一 UA %q：登录/聊天身份不一致会被上游看出混合身份（R51）",
					site.path, wantIdentityUA)
			}
			// 旧值不得残留（注释里提到旧值用于说明历史是允许的，但代码串不行）。
			for _, line := range strings.Split(src, "\n") {
				code := line
				if i := strings.Index(code, "//"); i >= 0 {
					code = code[:i]
				}
				if strings.Contains(code, `"CLI/2.63.2 CodeBuddy/2.63.2"`) {
					t.Errorf("%s 仍有旧 UA 代码串：%s", site.path, strings.TrimSpace(line))
				}
			}
		case "ver":
			if !strings.Contains(src, `"`+wantIdentityVer+`"`) {
				t.Errorf("%s 缺少 X-IDE-Version 版本号 %q（R51）", site.path, wantIdentityVer)
			}
			if strings.Contains(src, `"5.5.4"`) {
				t.Errorf("%s 仍有旧 X-IDE-Version 5.5.4（R51）", site.path)
			}
		case "prod":
			if !strings.Contains(src, `"`+wantIdentityProd+`"`) {
				t.Errorf("%s 缺少 X-Product %q（R51）", site.path, wantIdentityProd)
			}
		}
	}
}

// TestNoLegacyIdentityConstantsOutsideComments：全仓不得再出现旧身份串的
// **代码级**用法（字符串字面量在双引号里、且该行不是纯注释）。
func TestNoLegacyIdentityConstantsOutsideComments(t *testing.T) {
	root := repoRoot(t)
	var offenders []string
	_ = filepath.Walk(filepath.Join(root, "internal"), func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") {
			return nil
		}
		if strings.HasSuffix(p, "_test.go") {
			return nil // 测试里出现旧值是为了断言"不得出现"，不算违规
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		for _, line := range strings.Split(string(raw), "\n") {
			code := line
			if i := strings.Index(code, "//"); i >= 0 {
				code = code[:i]
			}
			if strings.Contains(code, `"CLI/2.63.2 CodeBuddy/2.63.2"`) ||
				strings.Contains(code, `"X-Machine-ID"`) || strings.Contains(code, `"X-Session-ID"`) {
				rel, _ := filepath.Rel(root, p)
				offenders = append(offenders, rel+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	for _, o := range offenders {
		t.Errorf("旧身份/自造指纹残留（R51）：%s", o)
	}
}

// repoRoot 从当前包目录向上找 go.mod。
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("找不到 go.mod（repoRoot 失效）")
	return ""
}
