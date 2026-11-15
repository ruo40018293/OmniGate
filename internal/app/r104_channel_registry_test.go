package app

// R104 渠道登记一致性闸门。
//
// 为什么要有这个文件：接一个新渠道需要同时改六处（provider.Kind、channelOrder、
// feeChannelOrder、noExplicitCheckin、stats.channelKind、前端 CH_LABEL/CH_CLASS/
// NO_EXPLICIT_CHECKIN/PROXY_CHANNELS/按钮）。R65 漏了 feeChannelOrder ⇒ 面板
// 「模型列表和费率」整组不出现，而 /v1/models 明明有；R103 盘点又发现 minimax /
// cline / kilo 三个渠道从未进过 stats.channelKind ⇒ 它们的 token 从来没进过平台表。
//
// 这类缺陷不会被任何运行时测试抓到（漏了就不出现，不报错），只能做源码级一致性检查。
// 本文件因此**读源文件**断言各登记点齐全——它测的不是业务逻辑，是「接线完整性」。
//
// readRepoFile / extractVarBlock 复用 fees_channel_order_test.go 里已有的实现。

import (
	"strings"
	"testing"
)

// r104Kinds 本轮接入的五个渠道：Kind 字面量 → 导出的常量名。
// 前端 class 与 Kind 同名是既有约定（CH_CLASS 里 13 个渠道全部同名）。
var r104Kinds = map[string]string{
	"zcode": "ZCode",
}

// r104Buttons 对应各渠道的添加按钮 id。每接入一个渠道就向
// r104Kinds 与 r104Buttons 同时添一项——两张表必须一直同步，否则测试会因“按钮数不对”而误报。
//
// 本期只接入 zcode，其余四个渠道（gemini / lobsterai / codearts / officeace）各自独立一期，
// 接入时同步向下面两张表添项即可。
var r104Buttons = map[string]string{
	"zcode": "btnAddZCode",
}

// TestR104KindsAreDeclaredInProvider provider.Kind 常量必须存在且字面量正确——
// 字面量是模型名前缀与 stats 归属的单一真源，写错一个字母等于新渠道永远匹配不到。
func TestR104KindsAreDeclaredInProvider(t *testing.T) {
	src := readRepoFile(t, "internal/provider/provider.go")
	for lit, exported := range r104Kinds {
		want := "\t" + exported + " Kind = \"" + lit + "\""
		if !strings.Contains(src, want) {
			t.Errorf("provider 缺少 %q（Kind 字面量必须与前端 class、stats 键完全一致）", want)
		}
	}
}

// TestR104KindsAreInChannelOrder channelOrder 决定账号卡片与费率表的固定顺序。
// 未登记不会报错，只是排到最后（按 100）；但那样 R104 五个渠道会散落在无序列里，
// 面板看起来「新渠道跑到最后去了」，所以显式要求登记。
func TestR104KindsAreInChannelOrder(t *testing.T) {
	src := readRepoFile(t, "internal/app/app.go")
	block := extractVarBlock(t, src, "channelOrder")
	for _, exported := range r104Kinds {
		if !strings.Contains(block, "provider."+exported+":") {
			t.Errorf("channelOrder 缺少 provider.%s", exported)
		}
	}
}

// TestR104KindsAreInFeeChannelOrder R65 的教训：漏了这张表 ⇒ 面板「模型列表和费率」
// 里整组不出现，而 /v1/models 明明有模型。用户看到的是「这个渠道没有模型」。
func TestR104KindsAreInFeeChannelOrder(t *testing.T) {
	src := readRepoFile(t, "internal/app/app.go")
	block := extractVarBlock(t, src, "feeChannelOrder")
	for _, exported := range r104Kinds {
		if !strings.Contains(block, "provider."+exported) {
			t.Errorf("feeChannelOrder 缺少 provider.%s：面板「模型列表和费率」里该渠道整组不出现（R65）", exported)
		}
	}
}

// TestR104KindsHaveNoManualCheckinButton 五个渠道本轮都没有可用的签到契约：
//   - ZCode 的 billing/claim 要 headful 浏览器 + 一次性验证码参数（DailyCheckin 恒返
//     ErrClaimUnsupported），给手动按钮就是给了个点了必失败的入口；
//   - 其余四个上游没有签到端点。
//
// 漏登记不会报错，只会让用户点到一个必然失败的按钮。
func TestR104KindsHaveNoManualCheckinButton(t *testing.T) {
	src := readRepoFile(t, "internal/app/app.go")
	idx := strings.Index(src, "func noExplicitCheckin(")
	if idx < 0 {
		t.Fatal("找不到 noExplicitCheckin 函数")
	}
	end := strings.Index(src[idx:], "\n}")
	if end < 0 {
		t.Fatal("找不到 noExplicitCheckin 函数体结束")
	}
	body := src[idx : idx+end]
	for _, exported := range r104Kinds {
		if !strings.Contains(body, "provider."+exported) {
			t.Errorf("noExplicitCheckin 缺少 provider.%s：面板会出现点了必失败的签到按钮", exported)
		}
	}
}

// TestR104KindsAreRegisteredInStats stats.channelKind 是平台统计表归属白名单：
// 漏登记 ⇒ 该渠道 token 从不进入平台表（R103 发现的 minimax/cline/kilo 就是这么漏的，
// 三者的 token 从来没被统计过）。
func TestR104KindsAreRegisteredInStats(t *testing.T) {
	src := readRepoFile(t, "internal/stats/stats.go")
	for _, exported := range r104Kinds {
		if !strings.Contains(src, "string(provider."+exported+")") {
			t.Errorf("stats.channelKind 缺少 string(provider.%s)：该渠道 token 不进平台统计表", exported)
		}
	}
}

// TestR104KindsAreRegisteredInFrontend 前端五张表 + 按钮必须齐全。
// 漏 CH_LABEL ⇒ 卡片显示原始 kind；漏 CH_CLASS ⇒ 没有渠道配色；漏
// NO_EXPLICIT_CHECKIN ⇒ 出现点了必失败的签到按钮；漏 PROXY_CHANNELS ⇒
// 设置里的代理列表找不到该渠道；漏按钮 ⇒ 用户根本没有添加入口。
func TestR104KindsAreRegisteredInFrontend(t *testing.T) {
	js := readRepoFile(t, "cmd/omnigate/web/app.js")
	html := readRepoFile(t, "cmd/omnigate/web/index.html")

	for lit := range r104Kinds {
		if !strings.Contains(js, lit+": \"") {
			t.Errorf("CH_LABEL / CH_CLASS / PROXY_HINT 缺少 %q 的条目", lit)
		}
		if !strings.Contains(js, `"`+lit+`"`) {
			t.Errorf("NO_EXPLICIT_CHECKIN 或 PROXY_CHANNELS 缺少 %q", lit)
		}
	}
	for lit, id := range r104Buttons {
		_ = lit
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html 缺少按钮 %s：用户没有添加入口", id)
		}
		if !strings.Contains(js, `$("`+id+`")`) {
			t.Errorf("app.js 未绑定 %s 的 onclick：按钮点了没反应", id)
		}
	}
}

// TestR104ButtonsAreUniqueInGrid 按钮的 grid 坐标不得重复——两个按钮占同一格会叠在
// 一起，用户只看得见上面那个，于是「某个渠道没有入口」这种误报就来了。
func TestR104ButtonsAreUniqueInGrid(t *testing.T) {
	html := readRepoFile(t, "cmd/omnigate/web/index.html")
	start := strings.Index(html, `<div class="pa-group pa-right">`)
	if start < 0 {
		t.Fatal("找不到 pa-group pa-right 容器")
	}
	end := strings.Index(html[start:], "\n        </div>")
	if end < 0 {
		t.Fatal("找不到容器结束标签")
	}
	block := html[start : start+end]

	seen := map[string]string{}
	for _, line := range strings.Split(block, "\n") {
		ci := strings.Index(line, "grid-column:")
		ri := strings.Index(line, "grid-row:")
		if ci < 0 || ri < 0 {
			continue
		}
		cut := func(s string) string {
			s = strings.TrimSpace(s)
			if i := strings.IndexAny(s, ";\""); i >= 0 {
				s = s[:i]
			}
			return s
		}
		key := cut(line[ri+len("grid-row:"):]) + "," + cut(line[ci+len("grid-column:"):])

		id := ""
		if i := strings.Index(line, `id="`); i >= 0 {
			if j := strings.Index(line[i+4:], `"`); j >= 0 {
				id = line[i+4 : i+4+j]
			}
		}
		if prev, ok := seen[key]; ok {
			t.Errorf("grid 坐标重复 row,col=%s：%s 与 %s 会叠在一起", key, prev, id)
		}
		seen[key] = id
	}
	// 13 个既有渠道 + 本轮新接入的（它们必须各自有一个按钮）。
	// 用动态阈值而不是写死数字：每接入一个渠道都要改一次硬编码阈值
	// 是设计漏洞——漏改的话测试会失效但还是绿的。
	if want := 13 + len(r104Buttons); len(seen) < want {
		t.Errorf("只识别到 %d 个按钮坐标，预期至少 %d 个（HTML 结构可能已变，本测试的分辨率失效）", len(seen), want)
	}
}
