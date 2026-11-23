//go:build !windows

package remotehost

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func listenSSHConfigTestAgent(t *testing.T) (string, net.Listener) {
	t.Helper()
	path := filepath.Join(shortSocketDir(t), "agent")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	return path, listener
}

func TestSSHConfigRejectsFIFO(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".ssh")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "config"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSSHConfig(); errorCode(err) != "ssh_config_invalid" {
		t.Fatalf("FIFO: %v", err)
	}
}
