//go:build !windows

package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"al.essio.dev/pkg/shellescape"
)

// Keep both HTTP clients, SSH, job control and archiving on the production
// path. Only the interactive CLI is a fixture: its signal handler records
// each actual continuation and stdin reports the kernel foreground group.
func TestPausedSessionHTTPGuestPTY(t *testing.T) {
	s, id, project := metadataFixture(t)
	s.ready = true
	s.projectShares = map[string]*projectShare{project: {done: make(chan struct{})}}
	home, err := filepath.EvalSymlinks(settledTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	installClaudeTestHelpers(t, bin)
	const cli = `#!/usr/bin/env python3
import json, os, pathlib, signal, sys
if sys.argv[1:3] == ["agents", "--json"]:
    print("[]")
    sys.exit()
home = pathlib.Path.home()
history = home / ".claude/projects/fixture" / (sys.argv[sys.argv.index("--session-id") + 1] + ".jsonl")
history.parent.mkdir(parents=True, exist_ok=True)
history.write_text(json.dumps({"uuid":"first","type":"user","cwd":os.getcwd(),"message":{"content":"original request"}}) + "\n")
def continued(*_):
    with (home / "continues").open("a") as log:
        print(os.getpid(), file=log)
signal.signal(signal.SIGCONT, continued)
(home / "cli-pid").write_text(str(os.getpid()))
print("ready for input", flush=True)
for line in sys.stdin:
    print("foreground group " + str(os.tcgetpgrp(0)), flush=True)
`
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(cli), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte("export PATH="+shellescape.Quote(bin)+":/usr/bin:/bin\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mobileTestSSHRewrite(t, s, home, []string{"HOME=" + home, "TMPDIR=" + home, "PATH=" + bin + ":/usr/bin:/bin"}, func(cmd string) string {
		// A real sshd owns the guest PTY. Keep that session leader separate
		// from the command, so closing this loopback SSH session hangs up the
		// foreground job exactly as closing a remote PTY does.
		if strings.Contains(cmd, "TERM=xterm-256color") {
			return "/bin/sh -c " + shellescape.Quote(cmd) + "; exit $?"
		}
		return cmd
	})
	t.Cleanup(s.terminals.closeAll)
	terminal, err := s.createTerminal(terminalRequest{Workspace: project, Kind: "claude", sharedID: id})
	if err != nil {
		t.Fatal(err)
	}
	sub := terminal.subscribe()
	defer sub.Close()
	readTerminalUntil(t, sub, "ready for input")
	data, err := os.ReadFile(filepath.Join(home, "cli-pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil || pid <= 0 {
		t.Fatal("invalid fixture PID", string(data), err)
	}
	mobile, desktop := "/api/mobile/cli/sessions/"+id, "/api/cli/sessions/"+id
	decode := func(response *httptest.ResponseRecorder) mobileSession {
		t.Helper()
		var view mobileSession
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &view) != nil {
			t.Fatalf("session response %d: %s", response.Code, response.Body.String())
		}
		return view
	}
	send := func(text string) {
		t.Helper()
		if err := terminal.enqueueInput(context.Background(), text); err != nil {
			t.Fatal(err)
		}
	}
	pause := func() {
		t.Helper()
		send("\x1a")
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if view := decode(request(s, "GET", mobile, "", "", true)); view.Paused && view.Running {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("Ctrl+Z was not observed in the session API")
	}
	pause()
	reconnected := decode(request(s, "POST", mobile+"/open", s.origin, `{"automatic":true}`, true))
	if !reconnected.Paused || reconnected.DesktopTerminalID != terminal.id || reconnected.CanSend == nil || *reconnected.CanSend {
		t.Fatalf("automatic reopen changed the suspended job: %+v", reconnected)
	}
	if len(reconnected.Turns) != 1 || reconnected.Turns[0].Prompt != "original request" {
		t.Fatalf("history unavailable during suspension: %+v", reconnected.Turns)
	}
	// Input admission tracks typed /login commands under the same lock. This
	// detects a queued write even if the PTY writer immediately drains it.
	send("/login\r")
	terminal.mu.Lock()
	admitted := !terminal.loginRequested.IsZero() || len(terminal.inputs) != 0
	terminal.mu.Unlock()
	if admitted {
		t.Fatal("desktop input was admitted during suspension")
	}
	logPath := filepath.Join(home, "continues")
	if _, err := os.Stat(logPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("automatic reopen sent SIGCONT: %v", err)
	}
	replies := make(chan *httptest.ResponseRecorder, 2)
	for _, path := range []string{desktop, mobile} {
		go func() { replies <- request(s, "POST", path+"/continue", s.origin, "{}", true) }()
	}
	for range 2 {
		view := decode(<-replies)
		if view.Paused || view.DesktopTerminalID != terminal.id || view.Revision <= reconnected.Revision ||
			view.CanSend == nil || *view.CanSend || len(view.Turns) != 1 || view.Turns[0].ID != reconnected.Turns[0].ID {
			t.Fatalf("continue lost the original job/history or enabled an unconnected channel: %+v", view)
		}
	}
	send("verify\r")
	readTerminalUntil(t, sub, "foreground group "+strconv.Itoa(pid))
	signals, err := os.ReadFile(logPath)
	if err != nil || string(signals) != strconv.Itoa(pid)+"\n" {
		t.Fatalf("expected one continuation of PID %d, got %q (%v)", pid, signals, err)
	}
	pause()
	archive := request(s, "POST", desktop+"/archive", s.origin, "{}", true)
	if archive.Code != 200 || s.terminals.get(terminal.id) != nil {
		t.Fatalf("archive failed to close the terminal: %d %s", archive.Code, archive.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("archiving left PID %d in the guest", pid)
}
