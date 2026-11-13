package cage

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/inetaf/tcpproxy"
)

// The target waits for EOF before replying. A lost FIN stalls the reply, and
// a premature full close drops it. The relay must also release its gate slot.
func TestProxyRelayReleasesFinishedConnection(t *testing.T) {
	for _, scheme := range []string{"socks5", "http"} {
		t.Run(scheme, func(t *testing.T) {
			target, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer target.Close()
			go func() {
				c, err := target.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				if body, err := io.ReadAll(c); err == nil && string(body) == "request" {
					_, _ = io.WriteString(c, "reply")
				}
			}()
			cfg := DefaultConfig()
			cfg.ProxyURL, _ = exitProbeProxy(t, scheme, target.Addr().String())
			gate, err := NewEgressGate(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer gate.Close("test complete")
			fixtureGrant(t, gate)

			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			client, err := net.Dial("tcp4", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			src, err := listener.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer src.Close()
			ctx := t.Context()
			relay := tcpproxy.DialProxy{DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return gate.dialDNS(ctx, "tcp", "ipv4.icanhazip.com:443")
			}}
			done := make(chan struct{})
			go func() { relay.HandleConn(src); close(done) }()
			_ = client.SetDeadline(time.Now().Add(3 * time.Second))
			if _, err := io.WriteString(client, "request"); err != nil {
				t.Fatal(err)
			}
			if err := client.(*net.TCPConn).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			if reply, err := io.ReadAll(client); err != nil || string(reply) != "reply" {
				t.Fatalf("reply after FIN = %q, %v", reply, err)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("finished relay still running")
			}
			if active := gate.Status().Connections; active != 0 {
				t.Fatalf("%d connections retained after relay exit", active)
			}
		})
	}
}
