//go:build !windows && !linux

package raccoon

// readOSVersion 在 macOS / 其它平台上返回空串。
//
// 官方 macOS 客户端能拿到 process.getSystemVersion()（形如 `14.5.0`），但我们没有
// 等价的免依赖调用（sw_vers 需要 exec 子进程，为一次头取值而 spawn 进程不划算）。
// 官方规则：空值 ⇒ 不发该头。**宁可少发一个头，也不编造一个版本号** ——
// 编造的值一旦与真实运行时不一致，反而是更明显的破绽。
func readOSVersion() string { return "" }
