//go:build darwin || windows

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/sylvinus/agent-vm/internal/appupdate"
	"github.com/sylvinus/agent-vm/internal/cage"
)

// Resolve the installation that supplied this CLI, including when the user's
// command is a symlink. Searching PATH could update a different installation.
func nativeUpdateLauncher(executable, platform string) (string, error) {
	resources := filepath.Dir(executable)
	parent := filepath.Dir(resources)
	switch platform {
	case "darwin":
		if filepath.Base(executable) == "cage" && filepath.Base(resources) == "Resources" && filepath.Base(parent) == "Contents" && strings.EqualFold(filepath.Ext(filepath.Dir(parent)), ".app") {
			return filepath.Join(parent, "MacOS", "Cage"), nil
		}
	case "windows":
		if strings.EqualFold(filepath.Base(executable), "cage.exe") && strings.EqualFold(filepath.Base(resources), "resources") {
			return filepath.Join(parent, "Cage.exe"), nil
		}
	}
	return "", errors.New("此命令来自独立二进制或源码构建。请通过原安装包或源码构建流程升级 Cage。")
}

func installHostUpdate(ctx context.Context, config string, release appupdate.State, stdin, stdout, stderr *os.File, assumeYes bool) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return err
	}
	launcher, err := nativeUpdateLauncher(executable, runtime.GOOS)
	if err != nil {
		return err
	}
	cmd := exec.Command(launcher, "--cli-update")
	cmd.Env = append(os.Environ(), cage.ConfigEnv+"="+config)
	if runtime.GOOS == "windows" {
		cmd.Env = append(cmd.Env, "CAGE_UPDATE_CALLER_PID="+strconv.Itoa(os.Getpid()))
	}
	cmd.Dir = os.TempDir()
	return runNativeUpdate(ctx, cmd, release.LatestVersion, stdin, stdout, stderr, assumeYes)
}

type nativeUpdateState struct {
	appupdate.State
	OfficialUpgrade bool `json:"officialUpgrade"`
}

func runNativeUpdate(ctx context.Context, cmd *exec.Cmd, expected string, stdin *os.File, stdout, stderr io.Writer, assumeYes bool) error {
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	defer output.Close()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("无法启动此安装的 Cage 更新器：%w", err)
	}
	// Closing stdout unblocks the scanner when Ctrl+C arrives. After approval
	// the native installer owns the transaction and must be allowed to roll back.
	interrupt := context.AfterFunc(ctx, func() { _ = output.Close() })
	defer interrupt()
	approved := false
	defer func() {
		_ = input.Close()
		_ = output.Close()
		if !approved && ctx.Err() != nil && runtime.GOOS == "darwin" {
			_ = cmd.Process.Signal(os.Interrupt)
		}
		// Windows replacement waits for this CLI to release its own executable.
		go cmd.Wait()
	}()
	report := hostUpdateProgress(stderr)
	scanner := bufio.NewScanner(output)
	for scanner.Scan() {
		var state nativeUpdateState
		if json.Unmarshal(scanner.Bytes(), &state) != nil {
			return errors.New("Cage 更新器返回了无效状态。")
		}
		switch state.Phase {
		case "current":
			fmt.Fprintln(stdout, "Cage 已是当前更新通道的最新版本。")
			return nil
		case "ready":
			if approved || expected == "" || state.LatestVersion != expected {
				return errors.New("待安装的版本与此次检查不一致，请重新运行 cage update。")
			}
			prompt := fmt.Sprintf("已验证 Cage %s。是否安装并重新打开 Cage？", expected)
			if state.OfficialUpgrade {
				prompt = fmt.Sprintf("已验证官方签名版本 Cage %s。安装将替换当前构建并重新打开 Cage，自定义的程序改动将被覆盖。是否继续？", expected)
			}
			if err := confirmHostUpdate(ctx, stdin, stderr, prompt, assumeYes); err != nil {
				return err
			}
			if _, err := io.WriteString(input, "INSTALL\n"); err != nil {
				return errors.New("安装确认未能送达 Cage 更新器。")
			}
			approved = true
		case "handed_off":
			if !approved {
				return errors.New("更新器未收到本次终端确认。请检查安装状态。")
			}
			fmt.Fprintf(stdout, "Cage %s 已交给安装程序。新版启动完成后，可用 cage version 核对。\n", expected)
			return nil
		case "manual", "waiting", "failed", "canceled":
			if strings.TrimSpace(state.Message) == "" {
				return errors.New("Cage 更新器未能继续安装。")
			}
			return errors.New(state.Message)
		case "idle", "checking", "available", "downloading", "downloaded", "verifying", "installing":
			report(state.State)
		default:
			return errors.New("Cage 更新器返回了无法识别的状态。")
		}
	}
	if ctx.Err() != nil && !approved {
		return ctx.Err()
	}
	if approved {
		return errors.New("尚未收到安装程序接手的确认。请查看安装结果，并使用 cage version 核对。")
	}
	return errors.New("Cage 更新器退出时未报告结果。")
}
