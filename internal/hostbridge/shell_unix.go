//go:build !windows

package hostbridge

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

func userShell() string {
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	return "/bin/sh"
}

func normalizeShell(raw string) (string, error) {
	switch name := strings.ToLower(strings.TrimSpace(raw)); name {
	case "", "default":
		return "", nil
	case "bash", "sh", "zsh":
		return name, nil
	default:
		return "", fmt.Errorf("unsupported shell %q; use default, bash, sh, or zsh", raw)
	}
}

// shellCommand runs the command in the requested shell (or the user's own
// shell when shell is empty), so it sees the same PATH and tools as their
// terminal.
func shellCommand(command, shell string) (*exec.Cmd, error) {
	env := userEnv()
	var program string
	switch shell {
	case "":
		program = userShell()
	case "sh":
		program = "/bin/sh"
	case "bash", "zsh":
		program = lookProgram(shell, env)
		if !filepath.IsAbs(program) {
			if found, err := exec.LookPath(shell); err == nil {
				program = found
			} else {
				return nil, fmt.Errorf("shell %q is not installed on this computer", shell)
			}
		}
	default:
		return nil, fmt.Errorf("unsupported shell %q; use default, bash, sh, or zsh", shell)
	}
	cmd := exec.Command(program, "-c", command)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}

// hideWindow does nothing here: programs open no window of their own.
func hideWindow(*exec.Cmd) {}

func killTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

const pathMark = "\x1ePATH\x1e"

// terminalPath is the PATH the user's terminal has. An application opened
// from the desktop is given only the system's own, without the directories
// the user's shell adds when it starts, where their tools usually are.
var terminalPath = sync.OnceValue(readTerminalPath)

func readTerminalPath() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.Command(userShell(), "-l", "-i", "-c", `printf '%s' "`+pathMark+`${PATH}`+pathMark+`"`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		return ""
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		killTree(cmd)
		<-done
	}
	// The shell's own start-up files may print around the value.
	_, rest, found := strings.Cut(out.String(), pathMark)
	path, _, closed := strings.Cut(rest, pathMark)
	if !found || !closed || !strings.Contains(path, "/") {
		return ""
	}
	return path
}

// userEnv is this process's environment, without the application's own
// variables and with the PATH of the user's terminal, for the programs run on
// their behalf.
func userEnv() []string {
	base := baseEnv()
	path := terminalPath()
	if path == "" {
		return base
	}
	env := make([]string, 0, len(base)+1)
	for _, entry := range base {
		if !strings.HasPrefix(entry, "PATH=") {
			env = append(env, entry)
		}
	}
	return append(env, "PATH="+path)
}

// lookProgram is the program called name in the first folder of the PATH in
// env that has it, or name itself when none does, for exec to look up on
// this process's own PATH.
func lookProgram(name string, env []string) string {
	path := ""
	for _, entry := range env {
		if value, found := strings.CutPrefix(entry, "PATH="); found {
			path = value
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate
		}
	}
	return name
}

func resolveHostCommand(_ context.Context, name string, env []string) (string, []string, error) {
	return lookProgram(name, env), nil, nil
}
