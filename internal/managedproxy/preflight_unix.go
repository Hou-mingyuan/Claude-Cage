//go:build !windows

package managedproxy

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func tryLockPreflight(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}

func unlockPreflight(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}

func ownedByCurrentUser(_ string, info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
