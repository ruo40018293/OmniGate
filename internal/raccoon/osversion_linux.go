//go:build linux

package raccoon

import (
	"os"
	"strings"
)

// readOSVersion 返回 x-client-os-version 的取值：Linux 发行版主版本号
// （Debian 13 → `13`，Ubuntu → `24.04`）。
//
// 只解析 /etc/os-release 的 VERSION_ID：Debian/Ubuntu/Alpine 都有该键，且值已是
// 「发行版主版本」语义，无需再切分。读不到时返回空串 ⇒ ApplyHeaders 按官方规则跳过该头，
// 比填一个猜出来的值更安全（官方对空值也是不发）。
//
// 为什么不用内核串（os.release()）：官方 Electron 取的是 process.getSystemVersion()，
// 回落才是内核串；直接填内核串反而更像「真 Electron 跑在非桌面环境」的破绽。
func readOSVersion() string {
	for _, p := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if v := parseOSReleaseVersionID(string(b)); v != "" {
			return v
		}
	}
	// Linux 但读不到 os-release（极简容器）：宁可留空，也不编造发行版号。
	return ""
}

// parseOSReleaseVersionID 从 os-release 内容里取 VERSION_ID 的值（去引号、去尾空白）。
func parseOSReleaseVersionID(content string) string {
	for _, ln := range strings.Split(content, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(ln), "=")
		if !ok || strings.TrimSpace(k) != "VERSION_ID" {
			continue
		}
		return strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return ""
}
