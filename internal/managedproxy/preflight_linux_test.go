package managedproxy

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestStopPreflightProcessKillsOnlyTheRecordedCore(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep unavailable")
	}
	cmd := exec.Command(sleep, "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	started, bootID, err := preflightProcessStart(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	record := preflightCoreRecord{PID: cmd.Process.Pid, Started: started, BootID: bootID}
	alive := func() bool { return cmd.Process.Signal(syscall.Signal(0)) == nil }

	reused := record
	reused.Started++
	otherBoot := record
	otherBoot.BootID = "previous-boot"
	other, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for name, attempt := range map[string]func() error{
		"start time": func() error { return stopPreflightProcess(reused, sleep) },
		"boot":       func() error { return stopPreflightProcess(otherBoot, sleep) },
		"executable": func() error { return stopPreflightProcess(record, other) },
	} {
		if err := attempt(); err != nil || !alive() {
			t.Fatalf("%s mismatch: err=%v alive=%v", name, err, alive())
		}
	}

	resolved, _ := filepath.EvalSymlinks(sleep)
	if err := stopPreflightProcess(record, resolved); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("recorded core was not stopped")
	}
}
