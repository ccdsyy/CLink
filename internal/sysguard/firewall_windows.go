//go:build windows

package sysguard

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

const ruleName = "CLink P2P"

var (
	shell32          = syscall.NewLazyDLL("shell32.dll")
	procShellExecute = shell32.NewProc("ShellExecuteW")
)

func isAdmin() bool {
	// net session 需要管理员权限，执行失败即非管理员
	return exec.Command("net", "session").Run() == nil
}

func ensureFirewall() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	script := firewallScript(exe)
	if isAdmin() {
		out, err := exec.Command("cmd", "/c", script).CombinedOutput()
		if err != nil {
			return fmt.Errorf("防火墙设置失败：%s", string(out))
		}
		return nil
	}
	// UAC 提权：ShellExecuteW "runas"（弹出系统授权框，用户点"是"即完成）
	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString("cmd")
	args, _ := syscall.UTF16PtrFromString("/c " + script)
	r1, _, _ := procShellExecute.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(file)),
		uintptr(unsafe.Pointer(args)),
		0, 0, // SW_HIDE
	)
	if r1 <= 32 {
		return fmt.Errorf("未获得管理员授权（错误码 %d）", r1)
	}
	return nil
}

// firewallScript 幂等防火墙脚本：删旧 + 加程序规则 + 放行 25565。
func firewallScript(exe string) string {
	return fmt.Sprintf(
		`netsh advfirewall firewall delete rule name="%[1]s" & `+
			`netsh advfirewall firewall delete rule name="%[1]s 25565" & `+
			`netsh advfirewall firewall add rule name="%[1]s" dir=in action=allow program="%[2]s" & `+
			`netsh advfirewall firewall add rule name="%[1]s 25565" dir=in action=allow protocol=TCP localport=25565`,
		ruleName, exe,
	)
}
