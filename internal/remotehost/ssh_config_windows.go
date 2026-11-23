package remotehost

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func isLocalSSHConfigPath(filename string) bool {
	volume := filepath.VolumeName(filename)
	if len(volume) != 2 || volume[1] != ':' {
		return false
	}
	root, err := windows.UTF16PtrFromString(volume + "\\")
	if err != nil {
		return false
	}
	switch windows.GetDriveType(root) {
	case windows.DRIVE_FIXED, windows.DRIVE_REMOVABLE, windows.DRIVE_RAMDISK, windows.DRIVE_CDROM:
		return true
	}
	return false
}

func openSSHConfig(filename string) (*os.File, error) {
	// Exclude UNC, devices and mapped network drives before and after resolving
	// the user path (which may contain a junction or symlink).
	if !isLocalSSHConfigPath(filename) {
		return nil, failure("ssh_config_read", "SSH 配置需要位于本机磁盘")
	}
	resolved, err := filepath.EvalSymlinks(filename)
	if err != nil {
		return nil, failure("ssh_config_read", "无法读取本机 SSH 配置，请确认文件存在且可读")
	}
	if !isLocalSSHConfigPath(resolved) {
		return nil, failure("ssh_config_read", "SSH 配置需要位于本机磁盘")
	}
	f, err := os.Open(resolved)
	if err != nil {
		return nil, failure("ssh_config_read", "无法读取本机 SSH 配置，请确认文件存在且可读")
	}
	return f, nil
}
