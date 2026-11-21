package host

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/sylvinus/agent-vm/internal/durable"
)

// EnsurePrivateDir creates host-only storage and removes inherited access.
// Windows uses an explicit DACL; POSIX mode bits do not protect Windows files.
func EnsurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return errors.New("private storage must be a real directory")
	}
	return protectPrivate(path, true)
}

// ProtectFile restricts an existing file or local socket to the current user.
func ProtectFile(path string) error { return protectPrivate(path, false) }

// CheckPrivateFile rejects non-regular files and access by other local users.
func CheckPrivateFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("private record must be a regular file")
	}
	return checkPrivate(path, info)
}

// CreatePrivateFile creates path with data only if it does not exist. The
// content is written and flushed under a temporary name first, so a crash
// or power loss never leaves a partly written file under the final name.
// It returns an error satisfying errors.Is(err, os.ErrExist) when path exists.
func CreatePrivateFile(path string, data []byte) error {
	dir, name := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	f, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	temporary := f.Name()
	defer os.Remove(temporary)
	err = ProtectFile(temporary)
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	// A hard link publishes the complete file without replacing an existing one.
	if err = os.Link(temporary, path); err == nil {
		return durable.SyncDir(dir)
	} else if errors.Is(err, os.ErrExist) {
		return err
	}
	// File systems without hard links: claim the name, then replace it.
	claim, claimErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if claimErr != nil {
		return claimErr
	}
	if err = claim.Close(); err != nil {
		return err
	}
	if err = os.Rename(temporary, path); err != nil {
		return err
	}
	return durable.SyncDir(dir)
}
