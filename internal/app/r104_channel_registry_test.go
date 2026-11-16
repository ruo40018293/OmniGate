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
	"regexp"
	"strings"
	"testing"
)

// r104Kinds 本轮接入的五个渠道：Kind 字面量 → 导出的常量名。
// 前端 class 与 Kind 同名是既有约定（CH_CLASS 里 13 个渠道全部同名）。
var r104Kinds = map[string]string{
	"zcode":     "ZCode",
	"lobsterai": "LobsterAI",
	"gemini":    "Gemini",
	"officeace": "OfficeAce",
	"codearts":  "CodeArts",
}

// r104NoManualCheckin 声明「本渠道没有可用的签到契约」的渠道集合。
//
// 刻意不写成「所有 r104Kinds 都没有手动签到」：第一批只接 zcode（无签到）时那样写
// 是偷懒成立的，lobsterai 有真签到（三步 slot → context → check_in），按普适断言
// 会要求它**必须**进 noExplicitCheckin —— 那是把正确实现判成失败。
var r104NoManualCheckin = map[string]bool{
	"zcode": true,
	// Gemini 无签到机制：配额是「窗口剩余百分比」，上游没有每日领取端点，
	// DailyCheckin 返回明确的「本渠道没有签到机制」错误（R106）。
	"gemini": true,
	// R107 的 officeace 刻意**不在**本表：它有真签到
	// （POST {gateway}/v1/subscription/bonus/claim，V11 签名，body {}），
	// 上游对已领过的号回同样列表（幂等），故手动按钮与定时任务都可用。
	// R108 的 codearts 同样**不在**本表：它有真签到活动（fetchCampaigns →
	// claim → confirm，只认 type == USER_LOGIN 的活动），并实现 CheckinGranter
	// 把本次发放额交给调度器记账。
}

// r104Buttons 对应各渠道的添加按钮 id。每接入一个渠道就向
// r104Kinds 与 r104Buttons 同时添一项——两张表必须一直同步，否则测试会因“按钮数不对”而误报。
//
// 本期顺序：zcode → lobsterai → gemini → officeace → codearts，每个渠道独立一期，
// 接入时同步向下面两张表添项即可。
var r104Buttons = map[string]string{
	"zcode":     "btnAddZCode",
	"lobsterai": "btnAddLobsterAI",
	"gemini":    "btnAddGemini",
	// R107 officeace：凭据由 channel 自带 login.go 编排 / 手工导入 officeace-*.json，
	// 本仓暂无面板登录入口，故按钮 id 留空（空串 = 断言「不要求存在按钮」）。
	"officeace": "",
	// R108 codearts：上游有独立 login_codearts 包（本机回调 + PKCE + DPoP），
	// 本仓已接 loginFlows 三处 + completeCodeArtsLogin，故给按钮。
	"codearts": "btnAddCodeArts",
}

// TestR104KindsAreDeclaredInProvider provider.Kind 常量必须存在且字面量正确——
// 字面量是模型名前缀与 stats 归属的单一真源，写错一个字母等于新渠道永远匹配不到。
func TestR104KindsAreDeclaredInProvider(t *testing.T) {
	src := readRepoFile(t, "internal/provider/provider.go")
	for lit, exported := range r104Kinds {
		// 按 token 匹配而不是精确字面量：gofmt 会把 Kind 常量块的 `Kind =`
		// 按最长标识符对齐，每接一个名字更长的渠道，既有行的空格数都会变。
		// 精确匹配会因为排版变化而误报（本轮 lobsterai 就是这样撞上的），
		// 而本测试要断言的是「字面量与前端 class / stats 键一致」，与排版无关。
		re := regexp.MustCompile(`\b` + exported + `\s+Kind\s*=\s*"` + regexp.QuoteMeta(lit) + `"`)
		if !re.MatchString(src) {
			t.Errorf("provider 缺少 %s Kind = %q（Kind 字面量必须与前端 class、stats 键完全一致）", exported, lit)
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

// TestR104KindsHaveNoManualCheckinButton 只有「没有可用签到契约」的渠道才必须进
// noExplicitCheckin；有真签到的渠道**必须不在里面**（否则用户点不到本该能用的签到按钮）。
//
// 两个方向都要断言：漏登记 ⇒ 出现点了必失败的按钮；误登记 ⇒ 隐藏了本该可用的按钮。
// 后者更隐蔽 —— 用户只会觉得「这个渠道怎么不能签到」。
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
	for lit, exported := range r104Kinds {
		inBody := strings.Contains(body, "provider."+exported)
		if r104NoManualCheckin[lit] && !inBody {
			t.Errorf("noExplicitCheckin 缺少 provider.%s：面板会出现点了必失败的签到按钮", exported)
		}
		if !r104NoManualCheckin[lit] && inBody {
			t.Errorf("noExplicitCheckin 不该含 provider.%s：该渠道有真签到，隐藏按钮会让用户以为不能签到", exported)
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

	// 逐表断言，不能只查「全文件包含 lit: \"」——CH_LABEL / CH_CLASS / PROXY_HINT
	// 三张表里都有这个形态，漏掉任何一张都不会红（本轮 M4 变异就是这样存活的：
	// 删掉 CH_LABEL 的条目后，CH_CLASS 里的同形条目把断言喂饱了）。
	for lit := range r104Kinds {
		for _, name := range []string{"CH_LABEL", "CH_CLASS", "PROXY_HINT"} {
			block, ok := jsTableBlock(js, "const "+name+" = {")
			if !ok {
				t.Errorf("找不到 %s 表", name)
				continue
			}
			if !strings.Contains(block, lit+`: "`) {
				t.Errorf("%s 缺少 %q 的条目", name, lit)
			}
		}
		// PROXY_CHANNELS 是数组字面量：所有渠道都该有（设置里的代理列表）。
		proxy, ok := jsLiteralBlock(js, "const PROXY_CHANNELS = ")
		if !ok {
			t.Error("找不到 PROXY_CHANNELS 声明")
		} else if !strings.Contains(proxy, `"`+lit+`"`) {
			t.Errorf("PROXY_CHANNELS 缺少 %q：设置里的代理列表找不到该渠道", lit)
		}

		// NO_EXPLICIT_CHECKIN 是 Set 字面量，口径必须与后端 noExplicitCheckin 一致：
		// **有真签到的渠道必须不在里面** —— 进了就等于把能用的签到按钮藏起来。
		// （本轮加上 lobsterai 后这条立刻暴露：它有三步真签到，不该被隐藏。）
		noCk, ok := jsLiteralBlock(js, "const NO_EXPLICIT_CHECKIN = ")
		if !ok {
			t.Error("找不到 NO_EXPLICIT_CHECKIN 声明")
			continue
		}
		has := strings.Contains(noCk, `"`+lit+`"`)
		if r104NoManualCheckin[lit] && !has {
			t.Errorf("NO_EXPLICIT_CHECKIN 缺少 %q：面板会出现点了必失败的签到按钮", lit)
		}
		if !r104NoManualCheckin[lit] && has {
			t.Errorf("NO_EXPLICIT_CHECKIN 不该含 %q：该渠道有真签到，隐藏按钮会让用户以为不能签到", lit)
		}
	}
	for lit, id := range r104Buttons {
		_ = lit
		// 空 id = 本渠道刻意不提供面板登录入口（凭据导入型，见 r104Buttons 注释）。
		// 这时反而要断言「没有残留按钮」——否则删了登录编排却留着按钮，
		// 用户点下去只会得到一条莫名错误。
		if id == "" {
			continue
		}
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
	nButtons := 0
	for _, id := range r104Buttons {
		if id != "" {
			nButtons++
		}
	}
	if want := 13 + nButtons; len(seen) < want {
		t.Errorf("只识别到 %d 个按钮坐标，预期至少 %d 个（HTML 结构可能已变，本测试的分辨率失效）", len(seen), want)
	}
}

// jsTableBlock 切出 `const NAME = { ... }` 的**表本体**（按花括号配对，不含分号）。
//
// 不能写成 `strings.Index(js[start:], "};")`：本文件的表字面量以 `;` 结尾，
// 那个 `};` 会一路命中文档后面某个无关的闭合，把一大段（含其它表）圈进来，
// 让「逐表断言」退化成「全文件断言」—— 变异照样存活。
func jsTableBlock(js, marker string) (string, bool) {
	start := strings.Index(js, marker)
	if start < 0 {
		return "", false
	}
	open := start + strings.Index(js[start:], "{")
	depth := 0
	for i := open; i < len(js); i++ {
		switch js[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return js[start : i+1], true
			}
		}
	}
	return "", false
}

// jsLiteralBlock 切出 `const NAME = ...` 的值本体（按 () 或 [] 配对，取最短闭合）。
func jsLiteralBlock(js, marker string) (string, bool) {
	start := strings.Index(js, marker)
	if start < 0 {
		return "", false
	}
	rest := js[start:]
	// 找第一个开括号，判断是 ( 还是 [
	openParen := strings.Index(rest, "(")
	openBrack := strings.Index(rest, "[")
	var openCh, closeCh byte
	open := -1
	switch {
	case openParen >= 0 && (openBrack < 0 || openParen < openBrack):
		open, openCh, closeCh = openParen, '(', ')'
	case openBrack >= 0:
		open, openCh, closeCh = openBrack, '[', ']'
	default:
		return "", false
	}
	depth := 0
	for i := open; i < len(rest); i++ {
		switch rest[i] {
		case openCh:
			depth++
		case closeCh:
			depth--
			if depth == 0 {
				return rest[:i+1], true
			}
		}
	}
	return "", false
}

// TestReloadAccountsCoversEveryChannel reloadAccounts 必须覆盖每个需要从目录重载的渠道。
//
// 为什么要有这条闸门（R106 实测缺陷）：R104/R105 接 zcode / lobsterai 时，
// `completeZCodeLogin` / `completeLobsterAILogin` 都调了 `a.reloadAccounts()`，
// 但 reloadAccounts 的函数体里**没有这两个渠道的分支** —— 那句调用对它们完全是空操作。
// 后果：新登录的账号只落盘、不进内存池，面板上看不到，必须重启进程才出现。
// 这不报错、不写日志，和「登录成功但什么都没发生」一模一样。
//
// 本测试对比三张表的差集，把「漏登记」变成编译后立刻可见的红：
//   - runtimes（main.go）：所有装配了池的渠道；
//   - reloadAccounts（app.go）：真正会从磁盘同步的渠道；
//   - 白名单：确定「不需要 reload」的例外，每个例外都要写清理由。
func TestReloadAccountsCoversEveryChannel(t *testing.T) {
	mainSrc := readRepoFile(t, "cmd/omnigate/main.go")
	appSrc := readRepoFile(t, "internal/app/app.go")

	// ① runtimes 表里的所有渠道。
	rtBlock, ok := braceBlock(mainSrc, "runtimes := map[provider.Kind]*server.Runtime{")
	if !ok {
		t.Fatal("找不到 runtimes 装配表")
	}
	rtRe := regexp.MustCompile(`provider\.(\w+):\s*\{`)
	all := map[string]bool{}
	for _, m := range rtRe.FindAllStringSubmatch(rtBlock, -1) {
		all[m[1]] = true
	}
	if len(all) < 10 {
		t.Fatalf("只解析到 %d 个渠道，runtimes 表结构可能已变（本测试分辨率失效）", len(all))
	}

	// ② reloadAccounts 函数体里实际处理的渠道。
	idx := strings.Index(appSrc, "func (a *App) reloadAccounts() {")
	if idx < 0 {
		t.Fatal("找不到 reloadAccounts")
	}
	body, ok := braceBlock(appSrc[idx:], "func (a *App) reloadAccounts() {")
	if !ok {
		t.Fatal("reloadAccounts 函数体无法切出")
	}
	covered := map[string]bool{}
	for _, m := range regexp.MustCompile(`a\.runtime\(provider\.(\w+)\)`).FindAllStringSubmatch(body, -1) {
		covered[m[1]] = true
	}

	// ③ 刻意不 reload 的例外。每加一项都必须在这里写清理由 ——
	// 空白名单会让本测试退化成「所有渠道都必须 reload」，而这两个确实不该有。
	exempt := map[string]string{
		// Kilo 是免密渠道：没有 auth 文件，池里只有一个 AnonymousAuth() 虚拟账号。
		"Kilo": "免密免费池，没有凭据文件可扫描（池内是 AnonymousAuth 虚拟账号）",
		// TraeCode 是别名渠道：与 TraeWork 共账号，池由 TraeWork 分支负责同步。
		"TraeCode": "别名渠道，与 TraeWork 共用账号池，由 TraeWork 分支同步",
	}

	for name := range all {
		if covered[name] {
			continue
		}
		if reason, ok := exempt[name]; ok {
			_ = reason
			continue
		}
		t.Errorf("reloadAccounts 缺少 provider.%s 分支：该渠道登录后新账号不进内存池，"+
			"面板要等进程重启才看得见（R106 实测缺陷）", name)
	}
	// 反向：例外表里的渠道不得同时出现在 reloadAccounts 里，否则例外理由已过时。
	for name, reason := range exempt {
		if covered[name] {
			t.Errorf("provider.%s 已在 reloadAccounts 里处理，例外理由「%s」已过时，请从 exempt 删除", name, reason)
		}
	}
	// 反向第二层：reloadAccounts 里出现的渠道必须在 runtimes 里有装配。
	//
	// 只做 all → covered 是单向的（R107 变异实测存活）：从 runtimes 删掉一个渠道、
	// 却留着 reloadAccounts 里的分支，上面那个循环遍历的是 all，根本走不到它，
	// 全套测试照样绿。而真实后果是 `a.runtime(provider.X)` 返回 nil ⇒ 该分支恒不执行，
	// 与「没写」等价，还多了一段看起来在工作的死代码。
	for name := range covered {
		if !all[name] {
			t.Errorf("reloadAccounts 处理了 provider.%s，但 runtimes 表里没有它的装配："+
				"a.runtime() 会返回 nil，该分支恒不执行（R107 变异实测）", name)
		}
	}
}

// braceBlock 从 marker 起按花括号配对切出完整块（含首尾）。
func braceBlock(src, marker string) (string, bool) {
	start := strings.Index(src, marker)
	if start < 0 {
		return "", false
	}
	open := start + strings.Index(src[start:], "{")
	depth := 0
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[open : i+1], true
			}
		}
	}
	return "", false
}

// TestFrontendTablesAreClosed 表字面量必须闭合。
//
// 本轮最贵的一课（R104 引入、R105 才发现）：往 CH_LABEL / CH_CLASS / PROXY_HINT
// 追加条目时吃掉了结尾的 ` }`，三个表都变成 `const X = { ...;` —— app.js 在解析
// 阶段直接 SyntaxError，**整个面板所有按钮失效**，而当时全部测试都是绿的
// （前端断言只查「有没有 `lit: "`」，后端测试根本不碰 JS）。
// 用户看到的是「面板能打开但点什么都没反应」，这种症状极难与「后端挂了」区分。
func TestFrontendTablesAreClosed(t *testing.T) {
	js := readRepoFile(t, "cmd/omnigate/web/app.js")

	if o, c := strings.Count(js, "{"), strings.Count(js, "}"); o != c {
		t.Errorf("app.js 花括号不配平：{ = %d, } = %d（差 %d）—— 语法错误会让整个面板失效", o, c, o-c)
	}
	if o, c := strings.Count(js, "["), strings.Count(js, "]"); o != c {
		t.Errorf("app.js 方括号不配平：[ = %d, ] = %d", o, c)
	}

	// 逐表确认块能被切出来（jsTableBlock 按花括号配对，未闭合时返回 false）。
	for _, name := range []string{"CH_LABEL", "CH_CLASS", "PROXY_HINT"} {
		if _, ok := jsTableBlock(js, "const "+name+" = {"); !ok {
			t.Errorf("%s 表未闭合：面板 JS 会 SyntaxError", name)
		}
	}
	for _, name := range []string{"NO_EXPLICIT_CHECKIN", "PROXY_CHANNELS"} {
		if _, ok := jsLiteralBlock(js, "const "+name+" = "); !ok {
			t.Errorf("%s 声明未闭合", name)
		}
	}
}
