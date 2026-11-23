//go:build !windows

package remotehost

import (
	"os"

	"golang.org/x/sys/unix"
)

func openSSHConfig(filename string) (*os.File, error) {
	info, err := os.Stat(filename)
	if err != nil {
		return nil, failure("ssh_config_read", "无法读取本机 SSH 配置，请确认文件存在且可读")
	}
	if !info.Mode().IsRegular() || info.Size() > sshConfigMaxBytes {
		return nil, failure("ssh_config_invalid", "SSH 配置必须是不超过 1 MiB 的普通文件")
	}
	// O_NONBLOCK also prevents a swap to a FIFO between Stat and Open
	// from blocking the handler. The opened file is checked again before reading.
	f, err := os.OpenFile(filename, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, failure("ssh_config_read", "无法读取本机 SSH 配置，请确认文件存在且可读")
	}
	return f, nil
}
