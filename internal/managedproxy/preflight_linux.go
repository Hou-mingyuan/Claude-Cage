package managedproxy

import (
	"os"
	"path/filepath"
	"strings"
)

func preflightProcessStart(pid int) (uint64, string, error) {
	started, err := linuxProcessStart(pid)
	if err != nil {
		return 0, "", err
	}
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return 0, "", err
	}
	return started, strings.TrimSpace(string(bootID)), nil
}

// stopPreflightProcess uses the regular core's pidfd-pinned stop, which
// signals only a process whose start time, boot and executable all match.
func stopPreflightProcess(record preflightCoreRecord, core string) error {
	executable, err := filepath.EvalSymlinks(core)
	if err != nil {
		return err
	}
	return stopLinuxProcess(linuxCoreRecord{PID: record.PID, Executable: executable, Started: record.Started, BootID: record.BootID}, core)
}
