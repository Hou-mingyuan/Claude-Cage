//go:build !windows

package workbench

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func extractAccountDesktopSession(t *testing.T) string {
	t.Helper()
	const startMarker = "cat >\"$program\" <<'SESSION'\n"
	const endMarker = "\nSESSION\n"
	start := strings.Index(linuxAccountDesktop, startMarker)
	if start < 0 {
		t.Fatal("SESSION heredoc start not found in linux-account-desktop.sh")
	}
	rest := linuxAccountDesktop[start+len(startMarker):]
	end := strings.Index(rest, endMarker)
	if end < 0 {
		t.Fatal("SESSION heredoc end not found in linux-account-desktop.sh")
	}
	return rest[:end+1]
}

func TestAccountDesktopScriptsParse(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	for name, script := range map[string]string{
		"launcher": linuxAccountDesktop,
		"session":  extractAccountDesktopSession(t),
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(bash, "-n")
			cmd.Stdin = strings.NewReader(script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
		})
	}
}

func TestAccountDesktopSessionStopMarkerDistinguishesCleanStopFromCrash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	sessionScript := extractAccountDesktopSession(t)
	cases := []struct {
		name           string
		staleMarker    bool
		windowOpens    bool
		appBehavior    string
		action         string
		wantExit       int
		wantStopMarker bool
	}{
		{name: "clean-stop-via-marker", staleMarker: true, windowOpens: true, appBehavior: "wait", action: "stop-app-with-marker", wantExit: 0, wantStopMarker: false},
		{name: "crash-sigterm-without-marker", staleMarker: true, windowOpens: true, appBehavior: "wait", action: "sigterm-app-no-marker", wantExit: 143, wantStopMarker: false},
		{name: "crash-nonzero-clears-stale-marker", staleMarker: true, windowOpens: true, appBehavior: "exit23", action: "none", wantExit: 23, wantStopMarker: false},
		{name: "stop-during-startup-with-marker", staleMarker: false, windowOpens: false, appBehavior: "wait", action: "stop-app-with-marker", wantExit: 0, wantStopMarker: false},
		{name: "crash-during-startup-without-marker", staleMarker: true, windowOpens: false, appBehavior: "exit17", action: "none", wantExit: 17, wantStopMarker: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			base := filepath.Join(dir, "base")
			runtimeDir := filepath.Join(dir, "runtime")
			for _, p := range []string{base, runtimeDir} {
				if err := os.MkdirAll(p, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if tc.staleMarker {
				if err := os.WriteFile(filepath.Join(runtimeDir, "stopping"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			readyFile := filepath.Join(dir, "ready")
			xpropBody := `printf '_NET_CLIENT_LIST(WINDOW): window id # 0x200001\n'`
			if !tc.windowOpens {
				xpropBody = `printf '_NET_CLIENT_LIST(WINDOW): window id # \n'`
			}
			appBody := `trap - TERM; exec sleep 30`
			switch tc.appBehavior {
			case "exit23":
				appBody = `while [ ! -f "$TEST_READY" ]; do sleep 0.02; done; exit 23`
			case "exit17":
				appBody = `sleep 0.05; exit 17`
			}
			prelude := `python3() {
  for arg in "$@"; do
    case "$arg" in
      *client_runtime.py*) printf '%s\n' "` + filepath.Join(base, "code") + `"; return 0 ;;
    esac
  done
  cat >/dev/null
}
mcookie() { printf '0123456789abcdef0123456789abcdef\n'; }
xauth() { :; }
tigervncpasswd() { cat >/dev/null; printf pass; }
Xtigervnc() { printf '99\n' >&3; exec >/dev/null 2>&1 3>&-; exec sleep 30; }
dbus-update-activation-environment() { :; }
openbox() { :; }
xsettingsd() { :; }
gnome-keyring-daemon() { cat >/dev/null; }
gdbus() { printf '(false,)\n'; }
xprop() { ` + xpropBody + `; }
systemd-notify() { touch "$TEST_READY"; }
claude-desktop() { ` + appBody + `; }
`
			script := strings.ReplaceAll(sessionScript, "/usr/bin/python3", "python3")

			stopUnlocker := make(chan struct{})
			go func() {
				fifo := filepath.Join(runtimeDir, "unlock")
				for {
					select {
					case <-stopUnlocker:
						return
					default:
					}
					if fi, err := os.Stat(fifo); err == nil && fi.Mode()&os.ModeNamedPipe != 0 {
						f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
						if err == nil {
							_, _ = f.WriteString("test-key\n")
							_ = f.Close()
							return
						}
					}
					time.Sleep(10 * time.Millisecond)
				}
			}()
			defer close(stopUnlocker)

			cmd := exec.Command(bash, "-s", "--", base, "default")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Stdin = strings.NewReader(prelude + script)
			cmd.Env = append(os.Environ(),
				"RUNTIME_DIRECTORY="+runtimeDir,
				"NOTIFY_SOCKET="+filepath.Join(runtimeDir, "notify"),
				"TEST_READY="+readyFile,
			)
			var out lockedBuffer
			cmd.Stdout = &out
			cmd.Stderr = &out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}()

			if tc.action != "none" {
				waitTarget := readyFile
				if !tc.windowOpens {
					waitTarget = filepath.Join(runtimeDir, "app-pid")
				}
				deadline := time.Now().Add(10 * time.Second)
				for {
					if _, err := os.Stat(waitTarget); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("session did not reach %s: %s", waitTarget, out.String())
					}
					time.Sleep(15 * time.Millisecond)
				}
				if tc.staleMarker {
					if _, err := os.Stat(filepath.Join(runtimeDir, "stopping")); !os.IsNotExist(err) {
						t.Fatalf("stale stopping marker was not removed on startup: %v", err)
					}
				}
				pidRaw, err := os.ReadFile(filepath.Join(runtimeDir, "app-pid"))
				if err != nil {
					t.Fatalf("app-pid missing: %v", err)
				}
				appPID, err := strconv.Atoi(strings.TrimSpace(string(pidRaw)))
				if err != nil || appPID <= 0 {
					t.Fatalf("invalid app-pid %q: %v", pidRaw, err)
				}
				switch tc.action {
				case "stop-app-with-marker":
					if err := os.WriteFile(filepath.Join(runtimeDir, "stopping"), nil, 0600); err != nil {
						t.Fatal(err)
					}
					if err := syscall.Kill(appPID, syscall.SIGTERM); err != nil {
						t.Fatal(err)
					}
				case "sigterm-app-no-marker":
					if err := syscall.Kill(appPID, syscall.SIGTERM); err != nil {
						t.Fatal(err)
					}
				}
			}

			waitErr := cmd.Wait()
			gotCode := 0
			if waitErr != nil {
				var exitErr *exec.ExitError
				if errors.As(waitErr, &exitErr) {
					gotCode = exitErr.ExitCode()
				} else {
					t.Fatalf("unexpected wait error: %v %s", waitErr, out.String())
				}
			}
			if gotCode != tc.wantExit {
				t.Fatalf("exit code = %d, want %d (output: %s)", gotCode, tc.wantExit, out.String())
			}
			_, markerErr := os.Stat(filepath.Join(runtimeDir, "stopping"))
			if (markerErr == nil) != tc.wantStopMarker {
				t.Fatalf("stopping marker exists=%t, want %t", markerErr == nil, tc.wantStopMarker)
			}
		})
	}
}
