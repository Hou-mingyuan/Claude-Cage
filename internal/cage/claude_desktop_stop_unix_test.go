//go:build !windows

package cage

import (
	"bytes"
	"errors"
	"net"
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

func TestDesktopFinishMarksStoppingBeforeSignalingApp(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	dir := t.TempDir()
	// Missing runtime directory is ignored cleanly.
	missing := exec.Command(bash, "-c", desktopFinish, "desktop-finish.sh", filepath.Join(dir, "missing"), "cage-claude-desktop.service")
	if out, err := missing.CombinedOutput(); err != nil {
		t.Fatalf("missing runtime dir failed: %v %s", err, out)
	}

	runtimeDir := filepath.Join(dir, "runtime")
	if err := os.MkdirAll(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	seenMarker := filepath.Join(dir, "seen-marker")
	ready := filepath.Join(dir, "app-ready")
	// Start a dummy app that checks whether the stopping marker already exists
	// at the moment SIGTERM arrives.
	app := exec.Command(bash, "-c", `trap 'test -f "$RUNTIME/stopping" && printf yes > "$SEEN"; exit 143' TERM
touch "$READY"
while true; do sleep 0.05; done`)
	app.Env = append(os.Environ(), "RUNTIME="+runtimeDir, "SEEN="+seenMarker, "READY="+ready)
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	appDone := make(chan error, 1)
	go func() { appDone <- app.Wait() }()
	defer func() {
		_ = app.Process.Kill()
		<-appDone
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dummy app did not become ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	pid := strconv.Itoa(app.Process.Pid)
	if err := os.WriteFile(filepath.Join(runtimeDir, "app-pid"), []byte(pid+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fakeProc := filepath.Join(dir, "proc")
	if err := os.MkdirAll(filepath.Join(fakeProc, pid), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeProc, pid, "cgroup"), []byte("0::/system.slice/cage-claude-desktop.service\n"), 0600); err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(desktopFinish, `"/proc/$app/cgroup"`, `"`+fakeProc+`/$app/cgroup"`)
	script = strings.ReplaceAll(script, `[[ -d /proc/$app ]]`, `kill -0 "$app" 2>/dev/null`)
	finish := exec.Command(bash, "-c", script, "desktop-finish.sh", runtimeDir, "cage-claude-desktop.service")
	if out, err := finish.CombinedOutput(); err != nil {
		t.Fatalf("desktop-finish.sh failed: %v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(runtimeDir, "stopping")); err != nil {
		t.Fatalf("stopping marker not created: %v", err)
	}
	got, err := os.ReadFile(seenMarker)
	if err != nil || string(got) != "yes" {
		t.Fatalf("stopping marker was not written before SIGTERM: %q %v", got, err)
	}
}

func TestClaudeDesktopSessionStopMarkerDistinguishesCleanStopFromCrash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	cases := []struct {
		name           string
		staleMarker    bool
		appBehavior    string
		action         string
		wantExit       int
		wantStopMarker bool
	}{
		{name: "clean-stop-via-marker", staleMarker: true, appBehavior: "wait", action: "stop-app-with-marker", wantExit: 0, wantStopMarker: false},
		{name: "crash-sigterm-without-marker", staleMarker: true, appBehavior: "wait", action: "sigterm-app-no-marker", wantExit: 143, wantStopMarker: false},
		{name: "crash-nonzero-clears-stale-marker", staleMarker: true, appBehavior: "exit42", action: "none", wantExit: 42, wantStopMarker: false},
		{name: "sigterm-session-with-marker", staleMarker: false, appBehavior: "wait", action: "sigterm-session-with-marker", wantExit: 0, wantStopMarker: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			home := filepath.Join(dir, "home")
			if err := os.MkdirAll(home, 0700); err != nil {
				t.Fatal(err)
			}
			// Use a short /tmp path so the Unix domain socket fits macOS's 104-byte limit.
			runtimeDir, err := os.MkdirTemp("/tmp", "cage-ds-*")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
			filesSock, err := net.Listen("unix", filepath.Join(runtimeDir, "files.sock"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = filesSock.Close() })
			if tc.staleMarker {
				if err := os.WriteFile(filepath.Join(runtimeDir, "stopping"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			readyFile := filepath.Join(dir, "ready")
			appBody := `trap - TERM; exec sleep 30`
			if tc.appBehavior == "exit42" {
				appBody = `while [ ! -f "$TEST_READY" ]; do sleep 0.02; done; exit 42`
			}
			prelude := `dbus-update-activation-environment() { :; }
xauth() { :; }
tigervncpasswd() { cat >/dev/null; printf pass; }
Xtigervnc() { exec >/dev/null 2>&1; exec sleep 30; }
xdpyinfo() { :; }
openbox() { :; }
gnome-keyring-daemon() { cat >/dev/null; }
gdbus() { printf '(false,)\n'; }
xprop() { printf '_NET_CLIENT_LIST(WINDOW): window id # 0x200001\n'; }
systemd-notify() { touch "$TEST_READY"; }
stub_desktop_files() { exec >/dev/null 2>&1; exec sleep 30; }
stub_app() { ` + appBody + `; }
stub_python() { cat >/dev/null; }
`
			script := claudeDesktopSession
			script = strings.ReplaceAll(script, "/run/cage-desktop", runtimeDir)
			script = strings.ReplaceAll(script, "/usr/local/bin/cage-display-governor", filepath.Join(dir, "missing-governor"))
			script = strings.ReplaceAll(script, "/usr/local/bin/cage-desktop-files", "stub_desktop_files")
			script = strings.ReplaceAll(script, "/usr/local/bin/cage-input-session", "stub_app")
			script = strings.ReplaceAll(script, "/usr/bin/python3", "stub_python")
			script = strings.ReplaceAll(script, "command -v xsettingsd", "false")

			// Feed the unlock FIFO once the session script creates it.
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

			cmd := exec.Command(bash, "-s")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Stdin = strings.NewReader(prelude + script)
			cmd.Env = append(os.Environ(),
				"HOME="+home,
				"CAGE_DESKTOP_SESSION=1",
				"CAGE_NOTIFY_SOCKET="+filepath.Join(runtimeDir, "notify"),
				"DBUS_SESSION_BUS_ADDRESS=unix:path="+filepath.Join(runtimeDir, "bus"),
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

			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(readyFile); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("session did not reach ready: %s", out.String())
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
			case "sigterm-session-with-marker":
				if err := os.WriteFile(filepath.Join(runtimeDir, "stopping"), nil, 0600); err != nil {
					t.Fatal(err)
				}
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
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
