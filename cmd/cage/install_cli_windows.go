//go:build windows

package main

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

//go:embed terminal-path.ps1
var terminalPathScript []byte

func installTerminalCommand() int {
	exe, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	root := filepath.Dir(filepath.Dir(exe))
	if _, err := os.Stat(filepath.Join(root, "Cage.exe")); err != nil || !strings.EqualFold(filepath.Base(filepath.Dir(exe)), "resources") {
		return fail(fmt.Errorf("请使用完整 Cage 应用 resources 目录中的 cage.exe 安装终端命令"))
	}
	script, err := os.CreateTemp("", "cage-terminal-*.ps1")
	if err != nil {
		return fail(err)
	}
	defer os.Remove(script.Name())
	_, err = script.Write(terminalPathScript)
	closeErr := script.Close()
	if err != nil {
		return fail(err)
	}
	if closeErr != nil {
		return fail(closeErr)
	}
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return fail(err)
	}
	cmd := exec.Command(filepath.Join(system, "WindowsPowerShell", "v1.0", "powershell.exe"), "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script.Name(), "-InstallLocation", root)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err = cmd.Run(); err != nil {
		return fail(err)
	}
	environment, _ := windows.UTF16PtrFromString("Environment")
	send := windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW")
	_, _, _ = send.Call(0xffff, 0x001a, 0, uintptr(unsafe.Pointer(environment)), 2, 3000, 0)
	fmt.Println("已注册 Cage 终端命令，重新打开终端即可使用。")
	return 0
}
