//go:build !windows

package workbench

import (
	"context"
	"errors"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
)

func hostRevealCommand(ctx context.Context, platform, path string) *exec.Cmd {
	if platform == "darwin" {
		return exec.CommandContext(ctx, "/usr/bin/open", "-R", "--", path)
	}
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
	return exec.CommandContext(ctx, "gdbus", "call", "--session", "--dest", "org.freedesktop.FileManager1", "--object-path", "/org/freedesktop/FileManager1", "--method", "org.freedesktop.FileManager1.ShowItems", "[\""+uri+"\"]", "")
}

func revealHostFile(ctx context.Context, path string) error {
	if err := hostRevealCommand(ctx, runtime.GOOS, path).Run(); err == nil {
		return nil
	}
	if runtime.GOOS == "linux" {
		if exec.CommandContext(ctx, "gio", "open", filepath.Dir(path)).Run() == nil {
			return nil
		}
		return errors.New("无法打开本机文件管理器，请检查桌面环境和文件权限")
	}
	return errors.New("无法打开本机文件或文件管理器，请检查默认应用和文件权限")
}
