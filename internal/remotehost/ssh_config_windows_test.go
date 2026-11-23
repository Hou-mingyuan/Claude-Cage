//go:build windows

package remotehost

import (
	"net"
	"testing"

	"github.com/Microsoft/go-winio"
)

func listenSSHConfigTestAgent(t *testing.T) (string, net.Listener) {
	t.Helper()
	pipe := `\\.\pipe\cage-import-test-` + newID()
	listener, err := winio.ListenPipe(pipe, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return pipe, listener
}
