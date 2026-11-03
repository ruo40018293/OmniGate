// headers.go 小浣熊出站请求头：对齐官方客户端**发 LLM 请求的那一层**（R91，取代原先的裸奔形态）。
//
// 取证来源（2026-10-31 反编译）：商汤小浣熊-1.0.38-arm64.deb →
//
//	resources/box-agent-runtime/bin/box-agent-acp
//	（PyInstaller 打包的 Python 进程；manifest.json 声明 box-agent 0.9.8，内嵌 CPython 3.12.13。
//	  必须用 py3.12 解 marshal —— 3.13 会报 "bad marshal data"）
//	├─ box_agent/llm/openai_client.py  ← 构造 AsyncOpenAI(api_key, base_url, timeout)，对话从这里发出
//	├─ box_agent/llm/base.py           ← _auth_headers / _agent_headers / _request_headers
//	├─ box_agent/client_info.py        ← ClientInfo.headers_for_url（x-client-* 的唯一来源）
//	└─ openai/_base_client.py          ← SDK 的 user_agent / platform_headers（X-Stainless-*）
//
// **对话请求不走渲染层**，走的是上面那个 Python 子进程。所以官方那组头叫
// x-client-{name,platform,version,os-version,channel,device-id}（多出 os-version/channel，
// name 恒 raccoon-ai），UA 是 openai SDK 的 `AsyncOpenAI/Python 2.8.0`，不是 Electron 形态。
//
// 照抄官方的判定规则（不自作主张）：
//   - headers_for_url 对每个值都过 _clean_header_value（strip、必须全 ASCII、长度 ≤256），
//     空值跳过该头；
//   - device_id 官方**不给它赋值** ⇒ 官方实际不发该头，本渠道也不发
//     （headers_for_url 确实构造了它，只是值恒空，别误读成「官方没有」）；
//   - LLM 路径**不发** X-Raccoon-Language（box_agent 内 grep 零命中，那是渲染层专有）；
//   - X-Org-Code 在 LLM 路径**有值才发**（个人账号没有企业码）。
//
// ⚠️ 与上游实现的一处刻意差异（R91）：上游网关跑在 Linux 上，平台相关头是常量
// `desktop-linux-arm64` / `Linux` / `arm64`。我们的 exe 跑在**用户自己的 Windows/Mac 机器**上，
// 照抄 linux 常量反而是更明显的破绽，故这些头按 runtime 派生（见平台判定函数）。
package raccoon

import (
	"net/http"
	"runtime"
	"strings"
)

const (
	// OfficialUA 官方 LLM 请求的 User-Agent（openai SDK 的 BaseClient.user_agent 原文
	// f"{__class__.__name__}/Python {_version}" ⇒ AsyncOpenAI + 2.8.0）。
	OfficialUA = "AsyncOpenAI/Python 2.8.0"

	// sdkVersion / pyVersion 与官方包同源：openai 2.8.0、内嵌 CPython 3.12.13。
	sdkVersion = "2.8.0"
	pyVersion  = "3.12.13"

	// clientName 取官方 package.json 的 name；官方为空时回落 `raccoon`。
	clientName = "raccoon-ai"
	// clientVersion 官方 app.getVersion() 经 normalizeClientVersion 后**带 v 前缀**。
	clientVersion = "v1.0.38"
	// clientChannel 官方常量 BOX_AGENT_CLIENT_CHANNEL = 'official'。
	clientChannel = "official"
)

// stainlesOS 对齐 openai SDK 的 get_platform()：platform.system().lower() 后
// darwin→MacOS、windows→Windows、linux→Linux，其余 Unknown。
func stainlesOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "MacOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	}
	return "Unknown"
}

// stainlessArch 对齐 openai SDK 的 get_architecture()（已核对 2.8.0 源码）。
//
// SDK 判定顺序：arm64/aarch64→arm64；arm→arm；x86_64→x64；sys.maxsize≤2^32→x32；
// 否则 OtherArch(machine)，其 __str__ 就是 `other:<machine>`。
//
// 关键差异是 platform.machine() 在各平台的返回值：Linux/macOS 上 x86 64 位是
// `x86_64` ⇒ `x64`；**Windows 上 process 报的是 `AMD64`** ⇒ 落进 OtherArch ⇒
// 官方实际发出 `other:amd64`。这是照抄而非猜测——照抄一个真实客户端比纠正它更安全。
func stainlessArch() string {
	switch runtime.GOARCH {
	case "arm64":
		return "arm64"
	case "arm":
		return "arm"
	case "386":
		// 32 位进程 sys.maxsize = 2^31-1 ≤ 2^32 ⇒ SDK 返回 x32（machine 名不参与）。
		return "x32"
	case "amd64":
		if runtime.GOOS == "windows" {
			return "other:amd64" // platform.machine()=="AMD64" → OtherArch("amd64")
		}
		return "x64" // platform.machine()=="x86_64"
	}
	return "other:" + runtime.GOARCH
}

// clientPlatformOf 对齐官方 normalizeDesktopPlatform(platform, arch) 的输出形态
// `desktop-<macos|windows|linux|unknown>-<x86|arm|arm64|unknown>`。
//
// 官方输出集里**没有 x64**（桌面客户端不区分 64/32 位 x86，统一叫 x86），
// 故 amd64 与 386 都映射成 x86。注意这是从输出集推断的、**未做个案反编译验证**
// （缺 box_agent client_info.py 的 normalizeDesktopPlatform 源）：可观测的取值集合
// 是硬证据，具体分支映射是推断。
func clientPlatformOf() string {
	os := "unknown"
	switch runtime.GOOS {
	case "windows":
		os = "windows"
	case "darwin":
		os = "macos"
	case "linux":
		os = "linux"
	}
	arch := "unknown"
	switch runtime.GOARCH {
	case "amd64", "386":
		arch = "x86"
	case "arm64":
		arch = "arm64"
	case "arm":
		arch = "arm"
	}
	return "desktop-" + os + "-" + arch
}

// ApplyHeaders 把官方 LLM 请求的请求头一次设好。
//
// orgCode：企业版才有；个人账号传空串。**只在非空时发送**。
//
// osVersion：x-client-os-version 取值，由 gatewayOSVersion 提供；空串则不发该头。
//
// sessID / turnID：官方 `_agent_headers` 的关联头，非空才发。**不伪造 Title / Call-Kind**：
// 官方由宿主按会话标题与调用类型填，网关无对应语义，硬填会把主对话误标成内部辅助调用。
func ApplyHeaders(req *http.Request, accessToken, orgCode, osVersion, sessID, turnID string) {
	req.Header.Set("User-Agent", OfficialUA)
	req.Header.Set("Content-Type", "application/json")
	// SDK 的 platform_headers（openai/_base_client.py 里那 6 个固定头）。
	req.Header.Set("X-Stainless-Lang", "python")
	req.Header.Set("X-Stainless-Package-Version", sdkVersion)
	req.Header.Set("X-Stainless-OS", stainlesOS())
	req.Header.Set("X-Stainless-Arch", stainlessArch())
	req.Header.Set("X-Stainless-Runtime", "CPython")
	req.Header.Set("X-Stainless-Runtime-Version", pyVersion)
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	if v := cleanHeaderValue(orgCode); v != "" {
		req.Header.Set("X-Org-Code", v)
	}
	// x-client-*：官方 ClientInfo.headers_for_url，逐项 cleanHeaderValue 后非空才写。
	for _, h := range []struct{ name, value string }{
		{"X-Client-Name", clientName},
		{"X-Client-Platform", clientPlatformOf()},
		{"X-Client-Version", clientVersion},
		{"X-Client-OS-Version", osVersion},
		{"X-Client-Channel", clientChannel},
	} {
		if v := cleanHeaderValue(h.value); v != "" {
			req.Header.Set(h.name, v)
		}
	}
	// X-RACCOON-*：官方 _agent_headers，非空才发。
	if v := cleanHeaderValue(sessID); v != "" {
		req.Header.Set("X-RACCOON-Session-ID", v)
	}
	if v := cleanHeaderValue(turnID); v != "" {
		req.Header.Set("X-RACCOON-Turn-ID", v)
	}
}

// cleanHeaderValue 对齐官方 _clean_header_value（_MAX_HEADER_VALUE_LENGTH = 256）：
// strip 后必须全 ASCII 且长度 ≤256，否则视为空、不发送。
func cleanHeaderValue(value string) string {
	s := strings.TrimSpace(value)
	if s == "" || len(s) > 256 {
		return ""
	}
	for _, r := range s {
		if r > 127 {
			return ""
		}
	}
	return s
}
