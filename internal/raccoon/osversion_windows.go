//go:build windows

package raccoon

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// readOSVersion 返回 x-client-os-version 的取值：Windows 版本的 `Major.Minor.Build`。
//
// 官方客户端（Electron）取 `process.getSystemVersion()`，Windows 上就是
// `10.0.26100` 这种形态。这里用 kernel32 的 RtlGetVersion 直接拿真实版本号
// （GetVersionEx 在 Win8.1+ 未声明兼容性时会撒谎，官方 Electron 也不用它）。
func readOSVersion() string {
	proc := lazyRtlGetVersion()
	if proc == nil {
		return ""
	}
	var info osVersionInfoW
	info.Size = uint32(unsafe.Sizeof(info))
	// RtlGetVersion 成功返回 STATUS_SUCCESS(0)；非 0 说明调用失败，宁可不发该头。
	if ret, _, _ := proc.Call(uintptr(unsafe.Pointer(&info))); ret != 0 {
		return ""
	}
	if info.Major == 0 && info.Minor == 0 && info.Build == 0 {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d", info.Major, info.Minor, info.Build)
}

// osVersionInfoW 是 OSVERSIONINFOW 的 Go 镜像（字段顺序与大小必须一致）。
type osVersionInfoW struct {
	Size       uint32
	Major      uint32
	Minor      uint32
	Build      uint32
	PlatformID uint32
	CSDVersion [128]uint16
}

var (
	rtlOnce sync.Once
	rtlProc *windows.LazyProc
)

// lazyRtlGetVersion 惰性解析 kernel32!RtlGetVersion；解析失败返回 nil。
func lazyRtlGetVersion() *windows.LazyProc {
	rtlOnce.Do(func() {
		p := windows.NewLazySystemDLL("kernel32.dll").NewProc("RtlGetVersion")
		if err := p.Find(); err == nil {
			rtlProc = p
		}
	})
	return rtlProc
}
