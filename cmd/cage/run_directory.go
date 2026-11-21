package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sylvinus/agent-vm/internal/cli"
	"github.com/sylvinus/agent-vm/internal/workbench"
	"golang.org/x/term"
)

// followRunDirectory picks where `cage run` runs. Inside a shared project it
// runs from the same place in the environment. Anywhere else, or when that
// cannot be determined (Cage closed, environment not ready, project busy), it
// runs from the environment directory as it always did, after a one-line
// notice on stderr: scripts that call cage run from any directory keep
// working. An explicit --workdir is kept as given. --here asks for the strict
// behavior: run in the current directory or not at all, offering to share it
// when someone at the terminal can answer.
func followRunDirectory(configPath string, args []string, hostDir string, hostErr error, stderr io.Writer, confirm workbench.RunDirectoryConfirm) ([]string, error) {
	here, rest, err := cli.TakeRunFlag(args[1:], "--here")
	if err != nil {
		// The run reports its own usage errors.
		return args, nil
	}
	plain := append([]string{"run"}, rest...)
	workdir, _, _, command, err := cli.SplitRun(rest)
	if err != nil || len(command) == 0 {
		return plain, nil
	}
	if workdir != "" {
		if here {
			return nil, errors.New("--here 与 --workdir 不能同时使用")
		}
		return plain, nil
	}
	fallback := func(reason string, unshared bool) ([]string, error) {
		fmt.Fprintf(stderr, "cage run：%s，改在环境目录中运行。\n", reason)
		if unshared && confirm != nil {
			fmt.Fprintln(stderr, "  如需在当前目录运行：在 Cage 的“项目”中添加该文件夹，或使用 cage run --here 确认共享。")
		}
		return plain, nil
	}
	if hostErr != nil {
		if here {
			return nil, fmt.Errorf("无法读取当前目录：%w", hostErr)
		}
		return fallback("无法读取当前目录", false)
	}
	timeout := 30 * time.Second
	ask := workbench.RunDirectoryConfirm(nil)
	if here {
		// Sharing a folder and connecting it may take a while; the person
		// asked to wait for it.
		timeout, ask = 3*time.Minute, confirm
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	guest, err := runDirectory(ctx, configPath, hostDir, ask)
	if err != nil {
		if here {
			return nil, err
		}
		var reason *workbench.RunDirectoryError
		if errors.As(err, &reason) {
			return fallback(reason.Reason, reason.Unshared)
		}
		line, _, _ := strings.Cut(strings.TrimSpace(err.Error()), "\n")
		return fallback("无法确定当前目录在环境中的位置（"+line+"）", false)
	}
	return append([]string{"run", "--workdir", guest}, rest...), nil
}

// runDirectory is workbench.RunDirectory; a variable for tests.
var runDirectory = workbench.RunDirectory

// terminalConfirm asks on the terminal, when stdin and stderr are one; nil
// otherwise, so a script is never left waiting for an answer.
func terminalConfirm(stdin *os.File, stderr *os.File) workbench.RunDirectoryConfirm {
	if !term.IsTerminal(int(stdin.Fd())) || !term.IsTerminal(int(stderr.Fd())) {
		return nil
	}
	return func(folder string) (bool, error) { return askShare(stdin, stderr, folder) }
}

func askShare(in io.Reader, out io.Writer, folder string) (bool, error) {
	fmt.Fprintf(out, "当前目录尚未共享给 Cage：\n  %s\n共享后，环境中的程序可以读取和修改这个文件夹，包括其中的隐藏文件和 Git 仓库。\n是否共享并在其中运行？[y/N] ", folder)
	// Read one byte at a time: what follows the answer is the command's input.
	var line []byte
	buf := make([]byte, 1)
	for reads := 0; len(line) < 64 && reads < 256; reads++ {
		n, err := in.Read(buf)
		if n == 1 {
			if buf[0] == '\n' {
				break
			}
			line = append(line, buf[0])
			continue
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, err
		}
	}
	switch strings.ToLower(strings.TrimSpace(string(line))) {
	case "y", "yes", "是":
		return true, nil
	}
	return false, nil
}
