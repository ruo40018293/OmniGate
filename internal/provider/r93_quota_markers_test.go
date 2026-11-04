// r93_quota_markers_test.go —— 8 份平行 hardMarkers 的一致性闸门。
//
// 为什么要读源码文件而不是调函数：R52 定下的纪律是「渠道包之间不共享实现」，
// 于是 hardMarkers 在 qodercn/qoder/qodercom/qwenwork/upstream/workbuddyai/
// raccoon/loomy 各自维护一份（8 份）。它们都只做同一件事——把 body 归到
// ErrHardCredit——但没有公共接口可供断言，包内私有变量也无法从外部包访问。
//
// 2026-11-04 的事故正是「只改了一份就会漏」的形态：qodercn 收到
// `403 Billing daily count exceeded`，因为 8 份表里都没有这条文案，
// 它落到 ErrClient（可轮转）⇒ 单号渠道换号必败 ⇒ 503。
//
// 本测试把「必需文案」写死，任何一份表漏改都会在这里转红。
package provider

import (
	"os"
	"strings"
	"testing"
)

// 必需文案：日额度耗尽的已知形态（英文实测原文 + 中文官方提示）。
var requiredQuotaMarkers = []string{
	"billing daily count exceeded",
	"daily count exceeded",
	"次数已达上限",
}

// hardMarkerFiles 是 8 份平行实现的落点（相对 internal/provider）。
var hardMarkerFiles = []string{
	"../qodercn/client.go",
	"../qoder/client.go",
	"../qodercom/client.go",
	"../qwenwork/client.go",
	"../upstream/client.go",
	"../workbuddyai/models.go",
	"../raccoon/client.go",
	"../loomy/client.go",
}

func TestHardMarkerTablesCoverDailyQuota(t *testing.T) {
	for _, path := range hardMarkerFiles {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", path, err)
		}
		src := string(raw)
		if !strings.Contains(src, "hardMarkers") {
			t.Fatalf("%s 里找不到 hardMarkers —— 表被改名或搬走了，本闸门需要同步更新", path)
		}
		for _, m := range requiredQuotaMarkers {
			// 表里的写法带双引号，这里按带引号形式匹配，避免匹配到注释里的散字。
			if !strings.Contains(src, `"`+m+`"`) {
				t.Errorf("%s 的 hardMarkers 缺少 %q —— 日额度耗尽会被判成可轮转的请求错误", path, m)
			}
		}
	}
}
