//go:build !windows

package remotehost

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
)

// dialAgent connects to the explicit socket saved with the host, or to
// SSH_AUTH_SOCK when none is saved. The explicit socket must be a Unix socket
// owned by the current user that group and other users cannot write. The
// connection code does not read ssh_config or run ssh -G to discover one.
func dialAgent(ctx context.Context, explicit string) (net.Conn, error) {
	path := explicit
	if path == "" {
		path = os.Getenv("SSH_AUTH_SOCK")
		if path == "" {
			return nil, errors.New("SSH agent unavailable")
		}
	} else if err := checkAgentSocket(path); err != nil {
		return nil, err
	}
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}

func checkAgentSocket(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return failure("agent_socket_invalid", "指定的 SSH agent 套接字不存在或无法访问，请确认对应程序正在运行")
	}
	if info.Mode().Type() != os.ModeSocket {
		return failure("agent_socket_invalid", "指定的 SSH agent 路径不是 Unix 套接字")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() {
		return failure("agent_socket_invalid", "指定的 SSH agent 套接字不属于当前用户")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return failure("agent_socket_invalid", "指定的 SSH agent 套接字允许其他用户写入，请将权限改为仅自己可读写")
	}
	return nil
}
