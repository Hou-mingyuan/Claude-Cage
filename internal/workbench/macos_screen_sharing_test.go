package workbench

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/guestos"
	"golang.org/x/crypto/ssh"
)

func TestScreenSharingIsolationError(t *testing.T) {
	if err := screenSharingIsolationError(cage.GateStatus{Mode: cage.GateClosed}, nil); err != nil {
		t.Fatalf("closed gate should be isolated: %v", err)
	}
	for _, mode := range []string{cage.GateOpen, cage.GateChecking, "", "unknown"} {
		if !errors.Is(screenSharingIsolationError(cage.GateStatus{Mode: mode}, nil), errScreenSharingNotIsolated) {
			t.Errorf("mode %q should be refused", mode)
		}
	}
	if !errors.Is(screenSharingIsolationError(cage.GateStatus{Mode: cage.GateClosed}, errors.New("offline")), errScreenSharingGateUnknown) {
		t.Fatal("a gate read failure must be refused")
	}
}

func TestSetupSharingSatisfied(t *testing.T) {
	usable := macOSScreenSharingStatus{Usable: true, Authorized: cage.CheckUnknown}
	if setupSharingSatisfied(usable, false) {
		t.Fatal("usable but unverified authorization should still need the settings toggle")
	}
	if !setupSharingSatisfied(usable, true) {
		t.Fatal("confirming the settings toggle should complete first setup")
	}
	authorized := macOSScreenSharingStatus{Usable: true, Authorized: cage.CheckPassed}
	if !setupSharingSatisfied(authorized, false) {
		t.Fatal("an authorized usable session should pass without a repeated toggle")
	}
	denied := macOSScreenSharingStatus{Usable: false, Authorized: cage.CheckFailed}
	if setupSharingSatisfied(denied, true) {
		t.Fatal("a denied handshake must not be treated as complete")
	}
}

func TestAssembleScreenSharingStatus(t *testing.T) {
	denied := assembleScreenSharingStatus(true, true, nil, screenSharingRFBResult{
		Greeting:  "RFB 003.008",
		Handshake: true,
		Denied:    true,
		Reason:    "Screen Sharing is not allowed on localhost",
	}, nil)
	if denied.Usable || denied.State != cage.CheckFailed || denied.Authorized != cage.CheckFailed {
		t.Fatalf("denied handshake: %+v", denied)
	}
	if !strings.Contains(denied.Summary, "系统尚未允许") {
		t.Fatalf("denied summary: %s", denied.Summary)
	}

	ok := assembleScreenSharingStatus(true, true, nil, screenSharingRFBResult{
		Greeting:  "RFB 003.008",
		Handshake: true,
		Types:     2,
	}, nil)
	if !ok.Usable || ok.State != cage.CheckPassed || ok.Authorized != cage.CheckUnknown {
		t.Fatalf("successful handshake: %+v", ok)
	}
	if !strings.Contains(ok.Summary, "RFB 握手成功") || !strings.Contains(ok.Summary, "不被允许") {
		t.Fatalf("successful summary should still guide the toggle: %s", ok.Summary)
	}

	down := assembleScreenSharingStatus(false, false, nil, screenSharingRFBResult{Unreachable: true}, errScreenSharingUnreachable)
	if down.Usable || down.State != cage.CheckFailed {
		t.Fatalf("service down: %+v", down)
	}
}

func TestReadScreenSharingRFB(t *testing.T) {
	t.Run("security types", func(t *testing.T) {
		client, server := net.Pipe()
		defer client.Close()
		go func() {
			defer server.Close()
			_, _ = server.Write([]byte("RFB 003.008\n"))
			var echoed [12]byte
			if _, err := io.ReadFull(server, echoed[:]); err != nil {
				return
			}
			_, _ = server.Write([]byte{2, 30, 33})
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		result, err := readScreenSharingRFB(ctx, client)
		if err != nil || !result.Handshake || result.Denied || result.Types != 2 || result.Greeting != "RFB 003.008" {
			t.Fatalf("types: %+v err=%v", result, err)
		}
	})
	t.Run("security type list must be complete", func(t *testing.T) {
		wire := struct {
			io.Reader
			io.Writer
		}{
			bytes.NewReader(append([]byte("RFB 003.008\n"), 2, 30)), io.Discard,
		}
		result, err := readScreenSharingRFB(t.Context(), wire)
		if err == nil || result.Handshake {
			t.Fatalf("truncated type list accepted: %+v, %v", result, err)
		}
	})
	t.Run("RFB 3.3 single security type", func(t *testing.T) {
		wire := struct {
			io.Reader
			io.Writer
		}{
			bytes.NewReader(append([]byte("RFB 003.003\n"), 0, 0, 0, 30)), io.Discard,
		}
		result, err := readScreenSharingRFB(t.Context(), wire)
		if err != nil || !result.Handshake || result.Denied || result.Types != 1 {
			t.Fatalf("3.3 security type: %+v, %v", result, err)
		}
	})
	t.Run("not allowed reason", func(t *testing.T) {
		reason := "屏幕共享在 localhost 上不被允许"
		client, server := net.Pipe()
		defer client.Close()
		go func() {
			defer server.Close()
			_, _ = server.Write([]byte("RFB 003.008\n"))
			var echoed [12]byte
			_, _ = io.ReadFull(server, echoed[:])
			payload := append([]byte{0}, 0, 0, 0, 0)
			binary.BigEndian.PutUint32(payload[1:], uint32(len(reason)))
			_, _ = server.Write(append(payload, []byte(reason)...))
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		result, err := readScreenSharingRFB(ctx, client)
		if err != nil || !result.Handshake || !result.Denied || result.Reason != reason {
			t.Fatalf("denied: %+v err=%v", result, err)
		}
	})
	t.Run("hang up after version", func(t *testing.T) {
		client, server := net.Pipe()
		defer client.Close()
		go func() {
			_, _ = server.Write([]byte("RFB 003.008\n"))
			var echoed [12]byte
			_, _ = io.ReadFull(server, echoed[:])
			server.Close()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		result, err := readScreenSharingRFB(ctx, client)
		if err != nil || !result.Handshake || !result.Denied || !strings.Contains(result.Reason, "系统设置") {
			t.Fatalf("hang up: %+v err=%v", result, err)
		}
	})
}

func TestOpenMacOSScreenSharingActions(t *testing.T) {
	s := testServer(t)

	if w := screenSharingRequest(s, true, "{}"); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), errScreenSharingNotMacOS.Error()) {
		t.Fatalf("without a macOS guest: %d %s", w.Code, w.Body.String())
	}

	s.macosGuest = &guestos.MacOSProfile{Instance: "mac", User: "blue"}
	if w := screenSharingRequest(s, true, `{"action":"open"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), errScreenSharingNotReady.Error()) {
		t.Fatalf("not ready: %d %s", w.Code, w.Body.String())
	}

	s.ready = true
	var enabled int
	s.screenSharing = macOSScreenSharing{
		gate: func(context.Context) (cage.GateStatus, error) {
			return cage.GateStatus{Mode: cage.GateOpen}, nil
		},
		enable: func(context.Context) error {
			enabled++
			return nil
		},
		inspect: func(context.Context) (macOSScreenSharingStatus, error) {
			return assembleScreenSharingStatus(true, true, nil, screenSharingRFBResult{Greeting: "RFB 003.008", Handshake: true, Types: 1}, nil), nil
		},
	}

	if w := screenSharingRequest(s, true, `{"action":"enable"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), errScreenSharingNotIsolated.Error()) {
		t.Fatalf("enable with open gate: %d %s", w.Code, w.Body.String())
	}
	if enabled != 0 {
		t.Fatal("enable ran while the guest was not isolated")
	}

	s.screenSharing.gate = func(context.Context) (cage.GateStatus, error) {
		return cage.GateStatus{Mode: cage.GateChecking}, nil
	}
	if w := screenSharingRequest(s, true, `{"action":"enable"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), errScreenSharingNotIsolated.Error()) {
		t.Fatalf("enable while checking: %d %s", w.Code, w.Body.String())
	}

	s.screenSharing.gate = func(context.Context) (cage.GateStatus, error) {
		return cage.GateStatus{}, errors.New("missing")
	}
	if w := screenSharingRequest(s, true, `{"action":"enable"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), errScreenSharingGateUnknown.Error()) {
		t.Fatalf("enable with unknown gate: %d %s", w.Code, w.Body.String())
	}

	if w := screenSharingRequest(s, false, `{"action":"check"}`); w.Code != http.StatusForbidden {
		t.Fatalf("untrusted view: %d %s", w.Code, w.Body.String())
	}

	// Status checks also work while the gate is open.
	s.screenSharing.gate = func(context.Context) (cage.GateStatus, error) {
		return cage.GateStatus{Mode: cage.GateOpen}, nil
	}
	if w := screenSharingRequest(s, true, `{"action":"check"}`); w.Code != http.StatusOK {
		t.Fatalf("status check with gate open: %d %s", w.Code, w.Body.String())
	}
	var status macOSScreenSharingStatus
	if err := json.Unmarshal(screenSharingRequest(s, true, `{"action":"check"}`).Body.Bytes(), &status); err != nil || !status.Usable || status.State != cage.CheckPassed {
		t.Fatalf("status payload: %+v", status)
	}

	s.screenSharing.gate = func(context.Context) (cage.GateStatus, error) {
		return cage.GateStatus{Mode: cage.GateClosed}, nil
	}
	w := screenSharingRequest(s, true, `{"action":"enable"}`)
	if w.Code != http.StatusOK || enabled != 1 {
		t.Fatalf("isolated enable: %d %s enabled=%d", w.Code, w.Body.String(), enabled)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || !strings.Contains(status.Summary, "已重新启用") || !status.Usable {
		t.Fatalf("enable payload: %s", w.Body.String())
	}

	if w := screenSharingRequest(s, true, `{"action":"repair"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), errScreenSharingBadAction.Error()) {
		t.Fatalf("unknown action: %d %s", w.Code, w.Body.String())
	}
}

func TestOpenMacOSScreenSharingTunnel(t *testing.T) {
	s := testServer(t)
	connections, shutdown := startScreenSharingGuest(t, s)
	s.macosGuest = &guestos.MacOSProfile{Instance: "mac", User: "blue"}
	s.ready = true
	var opened []string

	s.screenSharing = macOSScreenSharing{
		gate: func(context.Context) (cage.GateStatus, error) {
			return cage.GateStatus{Mode: cage.GateClosed}, nil
		},
		open: func(url string) error {
			opened = append(opened, url)
			return nil
		},
	}

	w := screenSharingRequest(s, true, `{"action":"open"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("isolated open: %d %s", w.Code, w.Body.String())
	}
	var reply struct {
		OK      bool   `json:"ok"`
		Address string `json:"address"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil || !reply.OK || !strings.HasPrefix(reply.Address, "127.0.0.1:") {
		t.Fatalf("reply: %s", w.Body.String())
	}
	if len(opened) != 1 || opened[0] != "vnc://"+reply.Address {
		t.Fatalf("open url: %q", opened)
	}

	w = screenSharingRequest(s, true, `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("reuse: %d %s", w.Code, w.Body.String())
	}
	if connections.Load() != 1 {
		t.Fatalf("tunnel was recreated: starts=%d", connections.Load())
	}
	if len(opened) != 2 {
		t.Fatalf("reuse should reopen the client: %q", opened)
	}

	s.publishGate(cage.GateStatus{Mode: cage.GateOpen, RunID: "run", Epoch: 1})
	s.screenSharing.mu.Lock()
	alive := s.screenSharing.aliveLocked()
	port := s.screenSharing.port
	s.screenSharing.mu.Unlock()
	if !alive || port == 0 {
		t.Fatal("opening the gate tore down a daily-use tunnel")
	}

	s.screenSharing.gate = func(context.Context) (cage.GateStatus, error) {
		return cage.GateStatus{Mode: cage.GateOpen}, nil
	}
	w = screenSharingRequest(s, true, `{"action":"open"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("open after grant: %d %s", w.Code, w.Body.String())
	}
	if connections.Load() != 1 {
		t.Fatalf("grant recreated the tunnel: starts=%d", connections.Load())
	}

	s.closeMacOSScreenSharing()
	shutdown()
	w = screenSharingRequest(s, true, `{"action":"open"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), errScreenSharingUnreachable.Error()) {
		t.Fatalf("unreachable guest: %d %s", w.Code, w.Body.String())
	}
}

func TestMacOSSharingMaintenanceRequiresIsolation(t *testing.T) {
	s := testServer(t)
	s.macosGuest = &guestos.MacOSProfile{Instance: "mac", User: "blue"}
	s.ready = true
	s.screenSharing = macOSScreenSharing{gate: func(context.Context) (cage.GateStatus, error) {
		return cage.GateStatus{Mode: cage.GateOpen}, nil
	}}
	r := httptest.NewRequest(http.MethodPost, s.origin+"/api/macos/maintenance", strings.NewReader(`{"action":"open","maintenance":"sharing"}`))
	r.Header.Set("Origin", s.origin)
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
	r.AddCookie(&http.Cookie{Name: s.cookieName() + "-desktop", Value: trustedViewProof(s.token)})
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), errScreenSharingNotIsolated.Error()) {
		t.Fatalf("sharing maintenance with open gate: %d %s", w.Code, w.Body.String())
	}
}

func TestCloseMacOSScreenSharingOnStop(t *testing.T) {
	s := testServer(t)
	startScreenSharingGuest(t, s)
	s.macosGuest = &guestos.MacOSProfile{Instance: "mac", User: "blue"}
	s.ready = true
	rfb, err := s.inspectMacOSScreenSharingRFB(t.Context())
	if err != nil || !rfb.Handshake || rfb.Denied || rfb.Types != 1 {
		t.Fatalf("status via SSH stdio: %+v, %v", rfb, err)
	}
	port, err := s.ensureMacOSScreenSharingTunnel(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	done := s.screenSharing.done
	s.closeMacOSScreenSharing()
	select {
	case <-done:
	default:
		t.Fatal("stop returned before the SSH process was reaped")
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("the local forward still accepts connections after stop")
	}

}

func screenSharingRequest(s *Server, trusted bool, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, s.origin+"/api/environment/macos-screen-sharing", strings.NewReader(body))
	r.Header.Set("Origin", s.origin)
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
	if trusted {
		r.AddCookie(&http.Cookie{Name: s.cookieName() + "-desktop", Value: trustedViewProof(s.token)})
	}
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	return w
}

func writeScreenSharingSSHConfig(t *testing.T, s *Server, port int) {
	t.Helper()
	dir := filepath.Join(s.profile, "state", "lima", "mac")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := "Host lima-mac\n User cageprovision\n Port " + strconv.Itoa(port) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh.config"), []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
}

// Run the real OpenSSH client against an isolated server. Accept only the
// ordinary user and SSH forwarding channels aimed at guest loopback:5900.
func startScreenSharingGuest(t *testing.T, s *Server) (*atomic.Int32, func()) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("macOS guest tunnels use the host's Unix OpenSSH client")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	writeScreenSharingSSHConfig(t, s, ln.Addr().(*net.TCPAddr).Port)
	if err := os.WriteFile(filepath.Join(s.profile, "macos-known-hosts"), []byte("cage-macos "+string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), 0600); err != nil {
		t.Fatal(err)
	}
	var connections atomic.Int32
	var clients sync.WaitGroup
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			clients.Add(1)
			go func() {
				defer clients.Done()
				defer conn.Close()
				server, channels, requests, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				defer server.Close()
				connections.Add(1)
				if server.User() != "blue" {
					t.Errorf("unexpected SSH user %q", server.User())
					return
				}
				go ssh.DiscardRequests(requests)
				for incoming := range channels {
					var target struct {
						Host       string
						Port       uint32
						Origin     string
						OriginPort uint32
					}
					if incoming.ChannelType() != "direct-tcpip" || ssh.Unmarshal(incoming.ExtraData(), &target) != nil || target.Host != "127.0.0.1" || target.Port != 5900 {
						_ = incoming.Reject(ssh.Prohibited, "unexpected destination")
						continue
					}
					channel, requests, err := incoming.Accept()
					if err != nil {
						return
					}
					go ssh.DiscardRequests(requests)
					fmt.Fprint(channel, "RFB 003.889\n")
					var reply [12]byte
					if _, err = io.ReadFull(channel, reply[:]); err == nil && string(reply[:]) == "RFB 003.008\n" {
						_, _ = channel.Write([]byte{1, 30})
					}
					channel.Close()
				}
			}()
		}
	}()
	shutdown := func() {
		s.closeMacOSScreenSharing()
		ln.Close()
		<-stopped
		clients.Wait()
	}
	t.Cleanup(shutdown)
	return &connections, shutdown
}
