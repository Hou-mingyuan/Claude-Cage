package host

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// PrivateSocketDir returns a private local directory for the profile's
// AF_UNIX control sockets, with room more bytes available for socket names
// below it. Every process picks the same directory for a profile (see
// chooseWindowsSocketDir). With create, it is created when needed and
// restricted to the current user, and a failure to do so is reported rather
// than moving on to another directory other processes would not look in;
// without, nothing is created, so lookups never leave directories behind.
func PrivateSocketDir(profile string, room int, create bool) (string, error) {
	profile = filepath.Clean(profile)
	sum := sha256.Sum256([]byte(profile))
	home, _ := os.UserHomeDir()
	excluded := []string{os.Getenv("LOCALAPPDATA"), os.Getenv("APPDATA"), os.TempDir()}
	fs := windowsSocketFS{persistentACLs: volumePersistentACLs, exists: existingDir, mkdir: func(dir string) error { return os.MkdirAll(dir, 0700) }, shortName: shortPathName}
	dir, err := chooseWindowsSocketDir(WindowsSocketDirCandidates(profile, home, hex.EncodeToString(sum[:6])), room, excluded, fs, create)
	if errors.Is(err, errSocketDirNotCreated) {
		return "", err
	}
	if err != nil {
		return "", fmt.Errorf("Cage 找不到可用于本机控制通道的私有目录：配置目录与用户目录需位于本地 NTFS 等支持权限设置的磁盘、路径不超过约 %d 个字符，并在含中文等非英文字符时提供 8.3 短文件名（%w）", windowsSocketPathLimit-room, err)
	}
	if create {
		if err := EnsurePrivateDir(dir); err != nil {
			return "", fmt.Errorf("Cage 无法将本机控制通道目录 %s 设为仅当前用户可访问：%w", dir, err)
		}
	} else if !existingDir(dir) {
		// Not created yet: the core is not running.
		return "", errSocketDirNotCreated
	}
	return dir, nil
}

// existingDir reports whether path is a real directory.
func existingDir(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir() && info.Mode()&(os.ModeSymlink|os.ModeIrregular) == 0
}

// volumePersistentACLs reports whether the volume holding path keeps access
// control lists. It depends only on the path and the volume, and the path
// need not exist. When the volume cannot be queried, it is assumed to.
func volumePersistentACLs(path string) bool {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return true
	}
	root := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumePathName(name, &root[0], uint32(len(root))); err != nil {
		return true
	}
	var flags uint32
	if err := windows.GetVolumeInformation(&root[0], nil, 0, nil, nil, &flags, nil, 0); err != nil {
		return true
	}
	return flags&windows.FILE_PERSISTENT_ACLS != 0
}

func shortPathName(path string) (string, error) {
	long, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	buffer := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetShortPathName(long, &buffer[0], uint32(len(buffer)))
	if err != nil {
		return "", err
	}
	if n == 0 || int(n) >= len(buffer) {
		return "", fmt.Errorf("no short name")
	}
	return windows.UTF16ToString(buffer[:n]), nil
}
