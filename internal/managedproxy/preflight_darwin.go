package managedproxy

import (
	"errors"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

var errPreflightProcessGone = errors.New("process not found")

func preflightProcessStart(pid int) (uint64, string, error) {
	// A missing PID yields an empty result rather than an error.
	raw, err := unix.SysctlRaw("kern.proc.pid", pid)
	if err != nil {
		return 0, "", err
	}
	if len(raw) == 0 {
		return 0, "", errPreflightProcessGone
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, "", err
	}
	if int(info.Proc.P_pid) != pid {
		return 0, "", errPreflightProcessGone
	}
	t := info.Proc.P_starttime
	return uint64(t.Sec)*1_000_000 + uint64(t.Usec), "", nil
}

func preflightProcessIs(record preflightCoreRecord, core string) (bool, error) {
	started, _, err := preflightProcessStart(record.PID)
	if errors.Is(err, errPreflightProcessGone) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if started != record.Started {
		return false, nil
	}
	args, err := unix.SysctlRaw("kern.procargs2", record.PID)
	if err != nil {
		// Exited between the two queries, or not ours to inspect.
		return false, nil
	}
	return sameExecutable(procargsExecutable(args), core), nil
}

func sameExecutable(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// macOS has no pidfd. PIDs are assigned in increasing order, so the PID would
// have to wrap around between this check and the signal to hit another process.
func stopPreflightProcess(record preflightCoreRecord, core string) error {
	ours, err := preflightProcessIs(record, core)
	if err != nil || !ours {
		return err
	}
	if err := unix.Kill(record.PID, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if ours, err := preflightProcessIs(record, core); err == nil && !ours {
			return nil
		}
	}
	return errors.New("Cage 代理进程未能及时停止")
}
