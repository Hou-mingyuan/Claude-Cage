package workbench

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sylvinus/agent-vm/internal/cage"
	"go.yaml.in/yaml/v3"
)

func transferMountServer(t *testing.T, running bool, mounted string) *Server {
	t.Helper()
	s := testServer(t)
	transfer, old := t.TempDir(), t.TempDir()
	transfer, _ = filepath.EvalSymlinks(transfer)
	old, _ = filepath.EvalSymlinks(old)
	if err := writePrivate(filepath.Join(s.profile, "environment.json"), cage.EnvironmentRecord{Directory: transfer, IdentityDirectory: old}); err != nil {
		t.Fatal(err)
	}
	if mounted == "current" {
		mounted = transfer
	} else if mounted == "old" {
		mounted = old
	}
	config := map[string]any{"mountType": "reverse-sshfs", "mounts": []any{map[string]any{"location": mounted, "mountPoint": guestPath(mounted), "writable": true, "sshfs": map[string]any{"sftpDriver": "builtin"}}}, "networks": []any{}}
	data, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(s.instanceDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(s.instanceDir(), "lima.yaml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if running {
		if err = writePrivate(filepath.Join(s.instanceDir(), "cage-running.json"), map[string]int{"pid": os.Getpid()}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestTransferMountMigrationRequiresRestartAtEveryEntry(t *testing.T) {
	for _, entry := range []string{"attach", "start", "up"} {
		t.Run(entry, func(t *testing.T) {
			s := transferMountServer(t, true, "old")
			s.ready = true
			s.exe = filepath.Join(t.TempDir(), "must-not-start")
			var err error
			switch entry {
			case "attach":
				err = s.attach()
			case "start":
				err = s.startEnvironment(false)
			case "up":
				err = s.runCage("up")
			}
			var failure *projectShareError
			if !errors.As(err, &failure) || failure.Recovery != "environment-restart" || s.ready {
				t.Fatalf("old live share was accepted: ready=%t error=%v", s.ready, err)
			}
			if !s.vmRunning() {
				t.Fatal("restart guard changed the running instance")
			}
		})
	}
	for _, state := range []struct {
		running bool
		mount   string
	}{{false, "old"}, {true, "current"}} {
		s := transferMountServer(t, state.running, state.mount)
		if err := s.requireCurrentLinuxTransferMount(); err != nil {
			t.Fatalf("compatible or stopped share was rejected: %v", err)
		}
	}
}

func TestStoppingEnvironmentWithNoSelectedProjectStillStopsInstance(t *testing.T) {
	s := transferMountServer(t, false, "current")
	s.exe = filepath.Join(t.TempDir(), "missing-stop-program.exe")
	if s.workspace() != "" {
		t.Fatal("fixture unexpectedly has a project")
	}
	// A missing executable is deliberate: it proves the lifecycle reached the
	// actual stop command, without starting or stopping any real environment.
	if err := s.stopEnvironment(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty selection bypassed the environment stop: %v", err)
	}
}

func TestStoppingEnvironmentDoesNotRequireTransferDirectory(t *testing.T) {
	s := transferMountServer(t, false, "current")
	var err error
	s.exe, err = exec.LookPath("go")
	if err != nil {
		t.Skip("Go executable is unavailable")
	}
	if err := os.Remove(s.environmentDir()); err != nil {
		t.Fatal(err)
	}
	// `go stop` is an inert failing command. An ExitError proves the process
	// actually ran from the private profile after the transfer directory vanished.
	var exit *exec.ExitError
	if err := s.stopEnvironment(); !errors.As(err, &exit) {
		t.Fatalf("missing transfer directory prevented the stop command: %v", err)
	}
}
