package remotehost

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func TestSSHConfigImportedHostAuthenticatesWithLocalAgent(t *testing.T) {
	agentPath, listener := listenSSHConfigTestAgent(t)
	keyring := agent.NewKeyring()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := keyring.Add(agent.AddedKey{PrivateKey: private}); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { defer c.Close(); _ = agent.ServeAgent(keyring, c) }(conn)
		}
	}()
	t.Setenv("SSH_AUTH_SOCK", agentPath)
	serverKey, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	serverConfig := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if !bytes.Equal(key.Marshal(), serverKey.PublicKey().Marshal()) {
			return nil, failure("test_key", "unexpected key")
		}
		return nil, nil
	}}
	serverConfig.AddHostKey(serverKey)
	sshListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sshListener.Close() })
	serverErrors := make(chan error, 3)
	go func() {
		for range 3 {
			conn, err := sshListener.Accept()
			if err != nil {
				serverErrors <- err
				return
			}
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			client, _, _, err := ssh.NewServerConn(conn, serverConfig)
			if err == nil {
				_ = client.Close()
			} else {
				_ = conn.Close()
			}
			serverErrors <- err
		}
	}()
	for _, option := range []string{"", ` IdentityAgent "` + agentPath + "\"\n", " IdentityAgent SSH_AUTH_SOCK\n"} {
		preview, err := parseSSHConfig(strings.NewReader("Host fixture\n IdentityFile C:/NEVER_OPEN\n"+option), "user")
		if err != nil || len(preview.Hosts) != 1 {
			t.Fatalf("preview: %+v %v", preview, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		method, cleanup, err := authentication(ctx, preview.Hosts[0], ConnectSecrets{})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if method == nil {
			t.Fatal("no agent authentication method")
		}
		conn, err := net.DialTimeout("tcp", sshListener.Addr().String(), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		client, _, _, err := ssh.NewClientConn(conn, sshListener.Addr().String(), &ssh.ClientConfig{User: "user", Auth: []ssh.AuthMethod{method}, HostKeyCallback: ssh.FixedHostKey(serverKey.PublicKey())})
		if err != nil {
			_ = conn.Close()
			cleanup()
			cancel()
			t.Fatal(err)
		}
		_ = client.Close()
		cleanup()
		cancel()
		if err := <-serverErrors; err != nil {
			t.Fatal(err)
		}
	}
	// An unavailable agent must report failure, never fall back to IdentityFile.
	t.Setenv("SSH_AUTH_SOCK", agentPath+"-missing")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err = authentication(ctx, HostProfile{Auth: AuthConfig{Kind: "agent"}}, ConnectSecrets{})
	if typed, ok := err.(*Error); !ok || typed.Code != "agent_unavailable" {
		t.Fatalf("unavailable agent: %v", err)
	}
}
