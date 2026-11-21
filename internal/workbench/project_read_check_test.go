//go:build linux

package workbench

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestProjectReadCheckDetectsStalledSSHFS mounts a private SSHFS share served
// by a local sftp-server, stops the server, and runs the exact guest command.
func TestProjectReadCheckDetectsStalledSSHFS(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the check runs in the Linux guest")
	}
	server := "/usr/lib/openssh/sftp-server"
	for _, tool := range []string{"sshfs", "fusermount3", "mountpoint", "timeout", "/usr/bin/python3", server} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable")
		}
	}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("FUSE unavailable")
	}
	dir := t.TempDir()
	source, mount, pidFile := filepath.Join(dir, "source"), filepath.Join(dir, "mount"), filepath.Join(dir, "server.pid")
	for _, d := range []string{source, mount, filepath.Join(source, "sub")} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	wrapper := filepath.Join(dir, "transport")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\necho $$ > "+pidFile+"\nexec "+server+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sshfs", "-o", "ssh_command="+wrapper, "localhost:"+source, mount).CombinedOutput(); err != nil {
		t.Skipf("private SSHFS mount unavailable: %v %s", err, out)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGCONT)
		_ = exec.Command("fusermount3", "-uz", mount).Run()
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})
	run := func(path string) (time.Duration, error) {
		started := time.Now()
		err := exec.Command("/bin/bash", "-c", projectReadCommand(path, time.Second)).Run()
		return time.Since(started), err
	}
	if _, err := run(mount); err != nil {
		t.Fatal("healthy share failed the check:", err)
	}
	if _, err := run(source); err == nil {
		t.Fatal("a folder that is not mounted passed the check")
	}
	if _, err := os.ReadDir(mount); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	// The listing just read is cached, so a directory read would still pass.
	elapsed, err := run(mount)
	if err == nil {
		t.Fatal("stalled share passed the check")
	}
	if elapsed > 4*time.Second {
		t.Fatalf("stalled check took %s", elapsed)
	}
	if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	if _, err := run(mount); err != nil {
		t.Fatal("recovered share failed the check:", err)
	}
}
