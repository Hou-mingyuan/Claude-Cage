//go:build !windows

package durable

import (
	"errors"
	"os"
	"syscall"
)

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = f.Sync()
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	// Some file systems (network and FUSE mounts among them) do not sync
	// directories; the rename already happened and is not undone.
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) {
		return nil
	}
	return err
}
