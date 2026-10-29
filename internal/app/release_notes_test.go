package app

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestReleaseNotesArtifactNamesMatchReality 钉住「发布说明里写的文件名 = 实际发出去的文件名」。
//
// 为什么需要它：v0.1.5 的发布说明写着 `WorkBuddyAPI-0.1.5-<arch>.dmg`，而那次发布
// 实际产出的资产是 `OmniGate-0.1.5-amd64.dmg` / `-arm64.dmg` —— 改名发生在写说明之后。
// 于是 GitHub release 页面上留着一句「下载 WorkBuddyAPI-…dmg」，用户点进去根本找不到
// 这个文件。这类错误不会被构建、测试、lint 任何一环拦住，只能靠文档契约测试。
//
// 两个坑一起钉：
//  1. 旧产品名做文件名（改名后最容易漏的一处）；
//  2. `<arch>` 这种尖括号占位符 —— GitHub 会把 `…-0.1.5-<arch>.dmg` 里的 `<arch>`
//     当成未知 HTML 标签吃掉，页面上显示成 `…-0.1.5-.dmg`。文件名要写全，不写占位符。
//
// 只校验「当前版本」的说明文件（RELEASE-v<Version>.md）：历史版本的说明是历史事实
// （那几版确实叫旧名），回改等于篡改记录。
func TestReleaseNotesArtifactNamesMatchReality(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(root, "RELEASE-v"+Version+".md")
	raw, err := os.ReadFile(notes)
	if os.IsNotExist(err) {
		t.Skipf("%s 不存在（版本已 bump 但还没写说明），跳过", filepath.Base(notes))
	}
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)

	// ① 当前版本的发布说明里不能出现旧产品名当作文件名/包名。
	for _, legacy := range []string{"WorkBuddyAPI-", "WildWork-", "workbuddy-api.exe"} {
		if strings.Contains(text, legacy) {
			t.Errorf("%s 里出现了旧产品名 %q —— 改名后发布说明必须写新名（真实资产名见 release 页面）",
				filepath.Base(notes), legacy)
		}
	}

	// ② 尖括号占位符会被 GitHub 当 HTML 标签吞掉，必须写全架构名。
	if strings.Contains(text, "<arch>") {
		t.Errorf("%s 里用了 `<arch>` 占位符：GitHub 渲染时会当成 HTML 标签吃掉，页面上只剩 `-<ver>-.dmg`；请写全 amd64/arm64",
			filepath.Base(notes))
	}

	// ③ 出现的每个 dmg 名字必须等于本次实际会产出的名字。
	dmgRe := regexp.MustCompile(`[A-Za-z0-9._-]+\.dmg`)
	wantDmg := regexp.MustCompile(`^OmniGate-` + regexp.QuoteMeta(Version) + `-(amd64|arm64)\.dmg$`)
	names := dmgRe.FindAllString(text, -1)
	if len(names) == 0 {
		t.Fatalf("%s 没有提到任何 .dmg —— macOS 用户按说明找不到安装包", filepath.Base(notes))
	}
	for _, n := range names {
		if !wantDmg.MatchString(n) {
			t.Errorf("dmg 名 %q 与实际产物不符，应为 OmniGate-%s-{amd64,arm64}.dmg", n, Version)
		}
	}

	// ④ Windows 资产同理（同一类漏改：`.github/workflows/build.yml` 的产出名）。
	exeRe := regexp.MustCompile(`[A-Za-z0-9._-]+\.exe`)
	for _, n := range exeRe.FindAllString(text, -1) {
		if !strings.HasPrefix(n, "omnigate-") {
			t.Errorf("Windows 资产名 %q 与实际产物不符，应为 omnigate-windows-amd64.exe", n)
		}
	}
}
