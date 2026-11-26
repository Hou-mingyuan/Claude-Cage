package host

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

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
	fs := windowsSocketFS{persistentACLs: volumePersistentACLs, exists: existingDir, mkdir: func(dir string) error { return os.MkdirAll(dir, 0700) }, shortName: shortPathName, mkdirPrivate: mkdirPrivate, ownedPrivate: ownedPrivateDir}
	candidates := WindowsSocketDirCandidates(profile, home, hex.EncodeToString(sum[:6]))
	system := systemSocketDir(profile)
	if system != "" {
		candidates = append(candidates, system)
	}
	dir, err := chooseWindowsSocketDir(candidates, room, excluded, fs, create)
	if errors.Is(err, errSocketDirNotCreated) {
		return "", err
	}
	if err != nil {
		return "", fmt.Errorf("Cage 找不到可用于本机控制通道的私有目录：配置目录与用户目录需位于本地 NTFS 等支持权限设置的磁盘、路径不超过约 %d 个字符，并在含中文等非英文字符时提供 8.3 短文件名；系统盘根目录下的备用目录也无法使用。可通过 CLAUDE_CAGE_CONFIG 改用英文路径下的配置（%w）", windowsSocketPathLimit-room, err)
	}
	if dir == system {
		if err := openSharedSocketDir(dir, fs, create); errors.Is(err, errSocketDirNotCreated) {
			return "", err
		} else if err != nil {
			return "", fmt.Errorf("Cage 无法使用系统盘上的本机控制通道备用目录 %s：它须由当前用户创建且仅当前用户可访问。若该目录并非 Cage 为你创建，请由管理员确认后删除再重试（%w）", dir, err)
		}
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

// systemSocketDir is the profile's WindowsSystemSocketDir for the current
// user, or "" when the system drive or the user cannot be determined. The
// user's SID is the same in elevated and normal processes.
func systemSocketDir(profile string) string {
	windir, err := windows.GetSystemWindowsDirectory()
	if err != nil || len(windir) < 2 || windir[1] != ':' {
		return ""
	}
	sid, err := privateSID()
	if err != nil {
		return ""
	}
	return WindowsSystemSocketDir(windir[:2], sid.String(), profile)
}

func mkdirPrivate(path string) error {
	sid, err := privateSID()
	if err != nil {
		return err
	}
	// Owner and DACL are part of the creation, so the directory never
	// carries the access the drive root passes on to other users' entries.
	sd, err := windows.SecurityDescriptorFromString("O:" + sid.String() + "D:P(A;OICI;FA;;;" + sid.String() + ")(A;OICI;FA;;;SY)")
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	sa := windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))
	return windows.CreateDirectory(name, &sa)
}

// ownedPrivateDir rejects path unless it is a directory, not a reparse point,
// owned by the current user and closed to other users. Attributes and
// security are read through one handle opened without following reparse
// points, so both describe the same object even if the name is replaced
// meanwhile.
func ownedPrivateDir(path string) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(name, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("not a real directory")
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return checkPrivateDescriptor(sd)
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
