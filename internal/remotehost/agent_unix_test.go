//go:build !windows

package remotehost

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh/agent"
)

// shortSocketDir keeps socket paths under sockaddr_un's limit; test temp
// directories on macOS and in CI are often too long for it.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if !strings.HasPrefix(dir, "/tmp/") {
		t.Fatalf("socket directory %q is outside /tmp", dir)
	}
	return dir
}

func serveAgent(t *testing.T, path string, serve func(net.Conn)) {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); serve(conn) }()
		}
	}()
}

func agentProfile(socket string) HostProfile {
	return HostProfile{Name: "agent", Host: "localhost", User: "cage", Auth: AuthConfig{Kind: "agent", AgentSocket: socket}}
}

func errorCode(err error) string {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.Code
	}
	return ""
}

func TestExplicitAgentSocketDistinguishesEmptyBrokenAndUnsafeAgents(t *testing.T) {
	dir := shortSocketDir(t)
	// SSH_AUTH_SOCK names an empty agent, as with macOS launchd's default.
	empty := filepath.Join(dir, "empty")
	serveAgent(t, empty, func(conn net.Conn) { _ = agent.ServeAgent(agent.NewKeyring(), conn) })
	t.Setenv("SSH_AUTH_SOCK", empty)
	ctx := context.Background()
	if _, _, err := authentication(ctx, agentProfile(""), ConnectSecrets{}); errorCode(err) != "agent_empty" {
		t.Fatalf("empty default agent: %v", err)
	}

	keyed := filepath.Join(dir, "keyed")
	ring := agent.NewKeyring()
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	if err := ring.Add(agent.AddedKey{PrivateKey: private}); err != nil {
		t.Fatal(err)
	}
	serveAgent(t, keyed, func(conn net.Conn) { _ = agent.ServeAgent(ring, conn) })
	method, done, err := authentication(ctx, agentProfile(keyed), ConnectSecrets{})
	if err != nil || method == nil {
		t.Fatalf("explicit agent socket: %v", err)
	}
	done()

	broken := filepath.Join(dir, "broken")
	serveAgent(t, broken, func(conn net.Conn) {
		buf := make([]byte, 5)
		_, _ = io.ReadFull(conn, buf)
		_, _ = conn.Write([]byte{0, 0, 0, 1, 5}) // SSH_AGENT_FAILURE
	})
	if _, _, err := authentication(ctx, agentProfile(broken), ConnectSecrets{}); errorCode(err) != "agent_error" {
		t.Fatalf("listing failure reported as %v", err)
	}

	if _, _, err := authentication(ctx, agentProfile(filepath.Join(dir, "missing")), ConnectSecrets{}); errorCode(err) != "agent_socket_invalid" {
		t.Fatalf("missing socket: %v", err)
	}
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := authentication(ctx, agentProfile(regular), ConnectSecrets{}); errorCode(err) != "agent_socket_invalid" {
		t.Fatalf("regular file accepted as socket: %v", err)
	}
	if err := os.Chmod(keyed, 0666); err != nil {
		t.Fatal(err)
	}
	if _, _, err := authentication(ctx, agentProfile(keyed), ConnectSecrets{}); errorCode(err) != "agent_socket_invalid" {
		t.Fatalf("group-writable socket accepted: %v", err)
	}
	// A symlink to a safe socket is followed, as OpenSSH does.
	if err := os.Chmod(keyed, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(keyed, link); err != nil {
		t.Fatal(err)
	}
	if _, done, err := authentication(ctx, agentProfile(link), ConnectSecrets{}); err != nil {
		t.Fatalf("symlinked socket: %v", err)
	} else {
		done()
	}

	t.Setenv("SSH_AUTH_SOCK", "")
	if _, _, err := authentication(ctx, agentProfile(""), ConnectSecrets{}); errorCode(err) != "agent_unavailable" {
		t.Fatalf("unset SSH_AUTH_SOCK: %v", err)
	}
}
