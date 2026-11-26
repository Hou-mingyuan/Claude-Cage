package managedproxy

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// longRunningProcess starts a process that outlives the test unless stopped.
func longRunningProcess(t *testing.T) (*exec.Cmd, string, <-chan struct{}) {
	t.Helper()
	name, args := "sleep", []string{"30"}
	if runtime.GOOS == "windows" {
		name, args = "ping", []string{"-n", "30", "127.0.0.1"}
	}
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s unavailable", name)
	}
	cmd := exec.Command(path, args...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-exited })
	return cmd, path, exited
}

func TestStopPreflightProcessKillsOnlyTheRecordedCore(t *testing.T) {
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
	default:
		t.Skip("no preflight process support")
	}
	cmd, core, exited := longRunningProcess(t)
	started, bootID, err := preflightProcessStart(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	record := preflightCoreRecord{PID: cmd.Process.Pid, Started: started, BootID: bootID}
	alive := func() bool {
		select {
		case <-exited:
			return false
		case <-time.After(200 * time.Millisecond):
			return true
		}
	}

	reused := record
	reused.Started++
	other, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	attempts := map[string]func() error{
		"start time": func() error { return stopPreflightProcess(reused, core) },
		"executable": func() error { return stopPreflightProcess(record, other) },
	}
	if bootID != "" {
		otherBoot := record
		otherBoot.BootID = "previous-boot"
		attempts["boot"] = func() error { return stopPreflightProcess(otherBoot, core) }
	}
	for name, attempt := range attempts {
		if err := attempt(); err != nil || !alive() {
			t.Fatalf("%s mismatch: err=%v alive=%v", name, err, alive())
		}
	}

	resolved, err := filepath.EvalSymlinks(core)
	if err != nil {
		t.Fatal(err)
	}
	if err := stopPreflightProcess(record, resolved); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("recorded core was not stopped")
	}
}
