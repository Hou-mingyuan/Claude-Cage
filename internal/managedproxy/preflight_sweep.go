package managedproxy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sylvinus/agent-vm/internal/host"
)

const (
	preflightDirPrefix  = "preflight-"
	preflightLockName   = "owner.lock"
	preflightRecordName = "core.json"
	// A directory is created a moment before its lock; never sweep one that
	// young without a lock file.
	preflightLockGrace = time.Minute
)

// preflightCoreRecord pins the temporary core by PID and start time; the
// executable is checked against the bundled core when it is stopped.
type preflightCoreRecord struct {
	PID     int    `json:"pid"`
	Started uint64 `json:"started"`
	BootID  string `json:"boot_id,omitempty"`
}

var stopPreflightCore = stopPreflightProcess

// lockPreflightDir holds an advisory lock for the directory's lifetime; the
// lock is released by the kernel if this process dies, which marks the
// directory as stale. The descriptor is close-on-exec, so the core never
// inherits it.
func lockPreflightDir(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, preflightLockName), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	locked, err := tryLockPreflight(f)
	if err == nil && !locked {
		err = errors.New("preflight lock already held")
	}
	if err == nil {
		err = host.ProtectFile(f.Name())
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func recordPreflightCore(dir string, pid int) error {
	started, bootID, err := preflightProcessStart(pid)
	if err != nil {
		return err
	}
	data, err := json.Marshal(preflightCoreRecord{PID: pid, Started: started, BootID: bootID})
	if err != nil {
		return err
	}
	return Write(filepath.Join(dir, preflightRecordName), data)
}

// SweepPreflight removes temporary preflight directories in profile left by
// a process that no longer runs, stopping the core recorded in each first.
func SweepPreflight(profile string) error {
	return sweepPreflight(Dir(profile), func(record preflightCoreRecord) error {
		core, err := preflightCorePath()
		if err != nil {
			return err
		}
		return stopPreflightCore(record, core)
	})
}

func sweepPreflight(parent string, stop func(preflightCoreRecord) error) error {
	entries, err := os.ReadDir(parent)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), preflightDirPrefix) {
			errs = append(errs, sweepPreflightDir(filepath.Join(parent, entry.Name()), stop))
		}
	}
	return errors.Join(errs...)
}

func sweepPreflightDir(dir string, stop func(preflightCoreRecord) error) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 || !ownedByCurrentUser(dir, info) {
		return nil
	}
	lockPath := filepath.Join(dir, preflightLockName)
	var lock *os.File
	if st, err := os.Lstat(lockPath); os.IsNotExist(err) {
		if time.Since(info.ModTime()) < preflightLockGrace {
			return nil
		}
	} else if err != nil {
		return err
	} else if !st.Mode().IsRegular() {
		return nil
	} else {
		if lock, err = os.OpenFile(lockPath, os.O_RDWR, 0); err != nil {
			return err
		}
		locked, err := tryLockPreflight(lock)
		if err != nil || !locked {
			_ = lock.Close()
			return err // the owner is alive
		}
	}
	if record, ok := readPreflightRecord(dir); ok {
		if err := stop(record); err != nil {
			if lock != nil {
				_ = lock.Close()
			}
			return err // keep the record to retry on the next start
		}
	}
	// Windows cannot delete the lock file while it is open. A directory is
	// never reused once its owner is gone, so releasing the lock here is safe.
	if lock != nil {
		_ = lock.Close()
	}
	return os.RemoveAll(dir)
}

func readPreflightRecord(dir string) (preflightCoreRecord, bool) {
	var record preflightCoreRecord
	path := filepath.Join(dir, preflightRecordName)
	if host.CheckPrivateFile(path) != nil {
		return record, false
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) > 4096 || json.Unmarshal(b, &record) != nil || record.PID <= 1 || record.Started == 0 {
		return record, false
	}
	return record, true
}

// procargsExecutable returns the exec path from a macOS kern.procargs2 value:
// a 32-bit argc followed by the NUL-terminated path.
func procargsExecutable(b []byte) string {
	if len(b) < 4 {
		return ""
	}
	path, _, ok := strings.Cut(string(b[4:]), "\x00")
	if !ok {
		return ""
	}
	return path
}
