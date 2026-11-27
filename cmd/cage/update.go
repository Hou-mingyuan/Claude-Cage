package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sylvinus/agent-vm/internal/appupdate"
	"github.com/sylvinus/agent-vm/internal/workbench"
	"golang.org/x/term"
)

type hostUpdateOptions struct{ check, assumeYes bool }

func parseHostUpdateArgs(args []string) (hostUpdateOptions, error) {
	var opts hostUpdateOptions
	for _, arg := range args {
		switch arg {
		case "--check":
			opts.check = true
		case "--yes", "-y":
			opts.assumeYes = true
		default:
			return opts, errors.New("Usage: cage update [--check] [--yes]")
		}
	}
	return opts, nil
}

func runHostUpdate(ctx context.Context, config string, opts hostUpdateOptions, stdin, stdout, stderr *os.File) error {
	fmt.Fprintln(stderr, "正在检查 Cage 应用更新…")
	release, err := appupdate.Check(ctx, config)
	if err != nil {
		return err
	}
	if release.Phase == "current" {
		fmt.Fprintf(stdout, "Cage %s 已是当前更新通道的最新版本。\n", release.CurrentVersion)
		return nil
	}
	fmt.Fprintf(stdout, "Cage %s 可升级到 %s。\n", release.CurrentVersion, release.LatestVersion)
	if opts.check {
		return nil
	}
	return installHostUpdate(ctx, config, release, stdin, stdout, stderr, opts.assumeYes)
}

func confirmHostUpdate(ctx context.Context, in *os.File, out io.Writer, prompt string, assumeYes bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	fmt.Fprint(out, prompt)
	if assumeYes {
		fmt.Fprintln(out, "（已使用 --yes 确认）")
		return nil
	}
	display, ok := out.(*os.File)
	if !term.IsTerminal(int(in.Fd())) || (ok && !term.IsTerminal(int(display.Fd()))) {
		fmt.Fprintln(out)
		return errors.New("非交互安装需要 --yes。仅检查版本请运行 cage update --check。")
	}
	fmt.Fprint(out, " [y/N] ")
	answer := make(chan bool, 1)
	go func() {
		line, err := bufio.NewReader(io.LimitReader(in, 64)).ReadString('\n')
		reply := strings.ToLower(strings.TrimSpace(line))
		answer <- err == nil && (reply == "y" || reply == "yes" || reply == "是")
	}()
	select {
	case ok := <-answer:
		if ok {
			return ctx.Err()
		}
		return errors.New("已取消安装。")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Downloads can produce a progress event for each block. Keep redirected
// terminal logs small while still reporting retries and all phase changes.
func hostUpdateProgress(out io.Writer) func(appupdate.State) {
	lastPhase, lastMessage, lastStep := "", "", -1
	return func(state appupdate.State) {
		step := -1
		if state.Phase == "downloading" && state.TotalBytes > 0 && state.ReceivedBytes >= 0 && state.ReceivedBytes <= state.TotalBytes {
			step = int(state.ReceivedBytes * 10 / state.TotalBytes)
		}
		if state.Phase == lastPhase && step == lastStep && state.Message == lastMessage {
			return
		}
		lastPhase, lastMessage, lastStep = state.Phase, state.Message, step
		message := workbench.HostCLIDiagnostic(state.Message)
		if message == "" {
			return
		}
		if step >= 0 {
			fmt.Fprintf(out, "%s %d%%\n", message, step*10)
		} else {
			fmt.Fprintln(out, message)
		}
	}
}
