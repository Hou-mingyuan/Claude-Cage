//go:build windows

package remotehost

import (
	"context"
	"github.com/Microsoft/go-winio"
	"net"
	"os"
	"strings"
)

// dialAgent connects to the explicit named pipe saved with the host, or to
// SSH_AUTH_SOCK and then the Windows OpenSSH agent pipe when none is saved.
// Saved values are validated as local \\.\pipe\ names when the host is stored.
func dialAgent(ctx context.Context, explicit string) (net.Conn, error) {
	if explicit != "" {
		return winio.DialPipeContext(ctx, explicit)
	}
	path := os.Getenv("SSH_AUTH_SOCK")
	if path == "" {
		path = `\\.\pipe\openssh-ssh-agent`
	}
	if strings.HasPrefix(path, `\\.\pipe\`) {
		return winio.DialPipeContext(ctx, path)
	}
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}
