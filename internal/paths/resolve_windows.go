package paths

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

var finalPathName = windows.GetFinalPathNameByHandle

// Resolve follows directory junctions as well as symbolic links. EvalSymlinks
// may leave an NTFS mount-point reparse record in its original DOS spelling.
// Ask the kernel which object the open handle actually refers to instead.
func Resolve(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	name := abs
	switch {
	case strings.HasPrefix(name, `\\?\`), strings.HasPrefix(name, `\\.\`):
	case strings.HasPrefix(name, `\\`):
		name = `\\?\UNC\` + name[2:]
	default:
		name = `\\?\` + name
	}
	ptr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return "", err
	}
	h, err := windows.CreateFile(ptr, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", &os.PathError{Op: "resolve", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, 512)
	for {
		n, err := finalPathName(h, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			if !nameQueryUnsupported(err) {
				return "", &os.PathError{Op: "resolve", Path: path, Err: err}
			}
			// Some third-party file system drivers (SFTP or cloud drives such as
			// RaiDrive, RAM disks) open the directory but never implement the
			// name query. Fall back to resolving it component by component.
			return filepath.EvalSymlinks(abs)
		}
		if n >= uint32(len(buf)) {
			buf = make([]uint16, n+1)
			continue
		}
		result := windows.UTF16ToString(buf[:n])
		if strings.HasPrefix(result, `\\?\UNC\`) {
			return `\\` + result[8:], nil
		}
		if strings.HasPrefix(result, `\\?\`) && len(result) >= 7 && result[5] == ':' {
			return result[4:], nil
		}
		return result, nil
	}
}

func nameQueryUnsupported(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_FUNCTION) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) || errors.Is(err, windows.ERROR_INVALID_PARAMETER)
}
