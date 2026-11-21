// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/lima-vm/lima/v2/pkg/osutil"
)

func TestProcessCreationTimeFromSnapshot(t *testing.T) {
	if os.Getenv("LIMA_TEST_PROCESS_CREATION_TIME_CHILD") == "1" {
		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			t.Fatal(err)
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessCreationTimeFromSnapshot$")
	cmd.Env = append(os.Environ(), "LIMA_TEST_PROCESS_CREATION_TIME_CHILD=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		if err := cmd.Wait(); err != nil {
			t.Error(err)
		}
	})
	for _, tc := range []struct {
		name string
		pid  int
	}{
		{"current", os.Getpid()},
		{"child", cmd.Process.Pid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(tc.pid))
			if err != nil {
				t.Fatal(err)
			}
			defer windows.CloseHandle(handle)
			var created, exited, kernel, user windows.Filetime
			if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
				t.Fatal(err)
			}
			got, ok := processCreationTimeFromSnapshot(tc.pid)
			if !ok || got != created.Nanoseconds() {
				t.Fatalf("snapshot creation time = %d, %v; GetProcessTimes = %d", got, ok, created.Nanoseconds())
			}
		})
	}
	if created, ok := processCreationTimeFromSnapshot(-1); ok {
		t.Fatalf("nonexistent PID found: %d", created)
	}
}

func TestReadPIDFileRejectsReusedWindowsPID(t *testing.T) {
	for _, boot := range []string{"missing", "unsupported", "current"} {
		t.Run(boot, func(t *testing.T) {
			original := bootSessionID
			bootSessionID = func() (string, error) { return "boot-1", nil }
			if boot == "unsupported" {
				bootSessionID = func() (string, error) { return "", osutil.ErrBootSessionNotSupported }
			}
			t.Cleanup(func() { bootSessionID = original })
			dir := t.TempDir()
			if boot == "current" {
				if err := writeBootSessionMarker(dir, "boot-1"); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"ha.pid", "qemu.pid"} {
				path := filepath.Join(dir, name)
				if err := os.WriteFile(path, fmt.Appendf(nil, "%d\n", os.Getpid()), 0o644); err != nil {
					t.Fatal(err)
				}
				// Model a file left by an earlier owner of this still-live PID.
				old := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
				if err := os.Chtimes(path, old, old); err != nil {
					t.Fatal(err)
				}
				if pid, err := ReadPIDFile(path); err != nil || pid != 0 {
					t.Fatalf("reused PID accepted: %d, %v", pid, err)
				}
				if _, err := os.Stat(path); err != nil || processExited(os.Getpid()) {
					t.Fatal("reader removed the marker or signaled its new owner", err)
				}
				// The next startup replaces the stale file with its real owner.
				if err := WritePIDFile(path, os.Getpid()); err != nil {
					t.Fatal(err)
				}
				if pid, err := ReadPIDFile(path); err != nil || pid != os.Getpid() {
					t.Fatalf("live owner treated as stale: %d, %v", pid, err)
				}
			}
		})
	}
}

func TestReadPIDFileRemovesExitedWindowsProcess(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ha.pid")
	if err := os.WriteFile(path, fmt.Appendf(nil, "%d\n", cmd.Process.Pid), 0o644); err != nil {
		t.Fatal(err)
	}
	if pid, err := ReadPIDFile(path); err != nil || pid != 0 {
		t.Fatalf("exited process accepted: %d, %v", pid, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("dead process marker not removed", err)
	}
}
