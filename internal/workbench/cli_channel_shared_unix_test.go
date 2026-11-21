//go:build !windows

package workbench

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Periodic session views and discovery must reuse the control connection
// rather than open an SSH session (and run the user's shell startup) per call.
func TestCLIChannelUsesSharedControlSession(t *testing.T) {
	for _, name := range []string{"python3", "bash"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skip(name + " is unavailable")
		}
	}
	s := testServer(t)
	if _, ok := s.account.access(); !ok {
		t.Skip("test server account is locked")
	}
	home, _ := filepath.EvalSymlinks(t.TempDir())
	tmpRoot, _ := filepath.EvalSymlinks(os.TempDir())
	if !strings.HasPrefix(home, tmpRoot) && !strings.HasPrefix(home, filepath.Clean(os.TempDir())) {
		t.Fatalf("guest home %s is outside the temporary directory", home)
	}
	var starts atomic.Int32
	command := func(ctx context.Context) *exec.Cmd {
		starts.Add(1)
		cmd := exec.CommandContext(ctx, "python3", "-I", "-u", "-c", guestSessionServer)
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
		return cmd
	}
	ctx := t.Context()
	t.Cleanup(s.controlSession.close)
	// Open the connection the way guestSessionCommand would; later requests reuse it.
	if _, sent, err := s.controlSession.run(ctx, ctx, command, "true"); err != nil || !sent {
		t.Fatalf("opening control connection: sent=%v err=%v", sent, err)
	}
	for round := 0; round < 3; round++ {
		var found []terminalConversation
		if err := s.runCLIChannel(ctx, "discover", map[string]any{"accounts": []string{"default"}, "workspaces": map[string]string{"/work": "/work"}}, &found); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if len(found) != 0 {
			t.Fatalf("empty guest home reported sessions: %#v", found)
		}
	}
	if starts.Load() != 1 {
		t.Fatalf("%d control connections were opened", starts.Load())
	}
	if _, err := os.Stat(filepath.Join(home, ".local/share/cage/cli-channel-code")); err != nil {
		t.Fatalf("helper did not run in the guest home: %v", err)
	}
}

func TestDesktopCodexRPCUsesSharedControlSession(t *testing.T) {
	for _, name := range []string{"python3", "bash"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skip(name + " is unavailable")
		}
	}
	s := testServer(t)
	home, _ := filepath.EvalSymlinks(t.TempDir())
	var starts atomic.Int32
	command := func(ctx context.Context) *exec.Cmd {
		starts.Add(1)
		cmd := exec.CommandContext(ctx, "python3", "-I", "-u", "-c", guestSessionServer)
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
		return cmd
	}
	ctx := t.Context()
	t.Cleanup(s.controlSession.close)
	if _, sent, err := s.controlSession.run(ctx, ctx, command, "true"); err != nil || !sent {
		t.Fatalf("opening control connection: sent=%v err=%v", sent, err)
	}
	if err := s.desktopCodexRPC(ctx, "sync", map[string]any{"bindings": map[string]any{}}, nil); err == nil {
		t.Fatal("an uninstalled connector ran")
	}
	if err := s.desktopCodexRPC(ctx, "install", nil, nil); err != nil {
		t.Fatalf("install: %v", err)
	}
	installed, err := os.ReadFile(filepath.Join(home, ".local/share/project-collaboration/connector.py"))
	if err != nil || string(installed) != desktopCodexHelper {
		t.Fatalf("connector not installed intact in the guest home: %v", err)
	}
	var endpoints []desktopCodexEndpoint
	for round := 0; round < 3; round++ {
		if err := s.desktopCodexRPC(ctx, "sync", map[string]any{"bindings": map[string]any{}}, &endpoints); err != nil || len(endpoints) != 0 {
			t.Fatalf("sync round %d: %v %#v", round, err, endpoints)
		}
	}
	if starts.Load() != 1 {
		t.Fatalf("%d control connections were opened", starts.Load())
	}
}
