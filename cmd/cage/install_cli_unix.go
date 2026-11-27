//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func installTerminalCommand() int {
	exe, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fail(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(err)
	}
	bin := filepath.Join(home, ".local", "bin")
	if existing, err := exec.LookPath("cage"); err == nil {
		target, _ := filepath.EvalSymlinks(existing)
		if target != exe {
			return fail(fmt.Errorf("已有同名命令 %s，未覆盖", existing))
		}
		fmt.Println(existing)
		return 0
	}
	if err = os.MkdirAll(bin, 0755); err != nil {
		return fail(err)
	}
	name := filepath.Join(bin, "cage")
	if err = os.Symlink(exe, name); err != nil {
		target, readErr := os.Readlink(name)
		if !errors.Is(err, os.ErrExist) || readErr != nil || target != exe {
			return fail(fmt.Errorf("无法安装 %s：%w", name, err))
		}
	}
	fmt.Println(name)
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(d) == bin {
			return 0
		}
	}
	// Use a literal home-relative entry so shell metacharacters in the
	// account name never become executable shell text.
	shell := filepath.Base(os.Getenv("SHELL"))
	var profile string
	switch shell {
	case "zsh":
		dir := os.Getenv("ZDOTDIR")
		if !filepath.IsAbs(dir) {
			dir = home
		}
		profile = filepath.Join(dir, ".zprofile")
	case "bash":
		// Bash reads the first of these. Creating .bash_profile ahead of
		// an existing .profile would silently disable the user's startup.
		profile = filepath.Join(home, ".profile")
		for _, name := range []string{".bash_profile", ".bash_login"} {
			candidate := filepath.Join(home, name)
			if _, err := os.Stat(candidate); err == nil {
				profile = candidate
				break
			}
		}
	default:
		fmt.Println("请将 ~/.local/bin 加入终端的 PATH。")
		return 0
	}
	const line = "export PATH=\"$HOME/.local/bin:$PATH\""
	data, err := os.ReadFile(profile)
	if err != nil && !os.IsNotExist(err) {
		return fail(err)
	}
	if !strings.Contains("\n"+string(data)+"\n", "\n"+line+"\n") {
		f, err := os.OpenFile(profile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return fail(err)
		}
		_, writeErr := fmt.Fprintln(f, "\n"+line)
		closeErr := f.Close()
		if err = errors.Join(writeErr, closeErr); err != nil {
			return fail(err)
		}
	}
	fmt.Println("重新打开登录终端后即可使用 cage。")
	return 0
}
