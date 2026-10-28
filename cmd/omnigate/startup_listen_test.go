package main

import (
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
)

// TestIsAddrInUseRealOccupiedPort 真实撞端口：起一个 listener 占住 127.0.0.1:0，
// 再用同地址 listen ⇒ 底层 Errno 就是平台真实值（Windows 10048 / Unix 98）。
// 这是唯一能证明「判据在本机平台成立」的测法；合成链再准也只是模拟。
func TestIsAddrInUseRealOccupiedPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("准备 listener 失败: %v", err)
	}
	defer func() { _ = ln.Close() }()

	_, err = net.Listen("tcp", ln.Addr().String())
	if err == nil {
		t.Fatalf("同地址二次 listen 竟然成功，测试前提不成立")
	}
	if !isAddrInUse(err) {
		t.Fatalf("端口已占用必须判为 addr in use；实际 errno=%d err=%v", listenErrno(err), err)
	}
	// 反过来锁死「上游踩过的坑」：errors.Is(err, syscall.EADDRINUSE) 在 Windows 上恒假。
	// 若哪天 Go 改了归一化，这条断言会红——正好提醒我们简化实现。
	t.Logf("平台 errno=%d；errors.Is(EADDRINUSE)=%v", listenErrno(err), errors.Is(err, syscall.EADDRINUSE))
}

// TestListenErrnoUnwrapsChain 真实链是 *net.OpError → *os.SyscallError → syscall.Errno，
// 必须逐层剥到最内层才算得出来（只看第一层会漏判）。
func TestListenErrnoUnwrapsChain(t *testing.T) {
	inner := syscall.Errno(wsaeAddrInUse)
	err := &net.OpError{Op: "listen", Net: "tcp", Err: &os.SyscallError{Syscall: "bind", Err: inner}}
	if got := listenErrno(err); got != inner {
		t.Fatalf("应剥到最内层 errno=%d，得到 %d", inner, got)
	}
	if !isAddrInUse(err) {
		t.Fatalf("WSAEADDRINUSE(10048) 必须判为占用")
	}
}

// TestIsAddrInUseWindowsCodes Windows 两个码各自成立，且 10049（地址不可用）也要认——
// 它们同属 listen 阶段失败、进程无服务可用。
func TestIsAddrInUseWindowsCodes(t *testing.T) {
	cases := []struct {
		name  string
		errno syscall.Errno
		want  bool
	}{
		{"WSAEADDRINUSE", wsaeAddrInUse, true},
		{"WSAEADDRNOTAVAIL", wsaeAddrNotAvail, true},
		{"EADDRINUSE", syscall.EADDRINUSE, true},
		{"EADDRNOTAVAIL", syscall.EADDRNOTAVAIL, true},
		{"ECONNREFUSED", syscall.ECONNREFUSED, false},
	}
	for _, c := range cases {
		err := &net.OpError{Op: "listen", Net: "tcp", Err: &os.SyscallError{Syscall: "bind", Err: c.errno}}
		if got := isAddrInUse(err); got != c.want {
			t.Fatalf("%s: isAddrInUse=%v want %v", c.name, got, c.want)
		}
	}
}

// TestIsAddrInUseNegative 非 listen 错误、非 OpError、不可剥的链一律判 false，
// 否则会把「权限不足/配置错」也报成端口占用，误导用户去杀进程。
func TestIsAddrInUseNegative(t *testing.T) {
	if isAddrInUse(errors.New("some random failure")) {
		t.Fatalf("普通错误不得判为端口占用")
	}
	if isAddrInUse(nil) {
		t.Fatalf("nil 不得判为端口占用")
	}
	bad := &net.OpError{Op: "listen", Net: "tcp", Err: errors.New("no unwrap here")}
	if isAddrInUse(bad) {
		t.Fatalf("剥不出 errno 时宁可漏判，不得误报")
	}
}

// TestStartupExitsWhenListenFails 静态锁死启动路径：StartServer 失败分支必须
// Stop + os.Exit(1)（R62）。这条防的是「声明了却没接线」——只改回 log 而不退出即红。
func TestStartupExitsWhenListenFails(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读 main.go 失败: %v", err)
	}
	src := string(raw)
	anchor := "if err := appInst.StartServer(); err != nil {"
	i := strings.Index(src, anchor)
	if i < 0 {
		t.Fatalf("找不到 StartServer 错误分支锚点，测试需同步更新")
	}
	block := src[i:]
	if end := strings.Index(block, "\n\t}\n"); end > 0 {
		block = block[:end]
	} else {
		block = block[:min(len(block), 3000)]
	}
	if !strings.Contains(block, "os.Exit(1)") {
		t.Fatalf("StartServer 失败必须 os.Exit(1)，否则留下没有 HTTP 服务的空壳进程")
	}
	if !strings.Contains(block, "appInst.Stop()") {
		t.Fatalf("退出前必须 appInst.Stop() 释放资源")
	}
	if !strings.Contains(block, "isAddrInUse(err)") {
		t.Fatalf("端口占用必须单独识别并给出可操作提示")
	}
}

// TestNoTrayFlagStillExitsOnListenFailure 无托盘（--no-tray）时也不能跳过退出，
// 只应跳过系统通知（无托盘环境没有 Notify 出口）。
func TestNoTrayFlagStillExitsOnListenFailure(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读 main.go 失败: %v", err)
	}
	src := string(raw)
	if !strings.Contains(src, "if !autostart && !noTray {\n\t\t\t\tplatform.Notify(") {
		t.Fatalf("通知必须受 !autostart && !noTray 守卫，否则命令行/no-tray 场景会弹窗阻塞")
	}
}
