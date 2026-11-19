package cage

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGateProxyAuthenticatesAndPreservesClosedGate(t *testing.T) {
	token := strings.Repeat("a", 64)
	handler, err := GateProxy(fixtureGate(t), token)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, credentials string
		want              int
	}{
		{"anonymous", "", http.StatusProxyAuthRequired},
		{"wrong token", "proxy:wrong", http.StatusProxyAuthRequired},
		{"workload", "proxy:" + token, http.StatusForbidden},
		{"existing session", "cage:" + token, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://198.18.0.254/", nil)
			if tc.credentials != "" {
				req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(tc.credentials)))
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != tc.want {
				t.Fatalf("got %d, want %d", response.Code, tc.want)
			}
			if tc.want == http.StatusProxyAuthRequired && response.Header().Get("Proxy-Authenticate") != `Basic realm="proxy"` {
				t.Fatalf("unexpected proxy challenge %q", response.Header().Get("Proxy-Authenticate"))
			}
		})
	}
}

func TestGateProxyConnectKeepsReplyAfterClientFIN(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		if body, err := io.ReadAll(buffer); err == nil && string(body) == "request" {
			_, _ = io.WriteString(conn, "reply")
		}
	}))
	defer upstream.Close()
	cfg := DefaultConfig()
	cfg.ProxyURL = upstream.URL
	g, err := NewEgressGate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close("test complete")
	fixtureGrant(t, g)
	token := strings.Repeat("a", 64)
	handler, err := GateProxy(g, token)
	if err != nil {
		t.Fatal(err)
	}
	broker := httptest.NewServer(handler)
	defer broker.Close()
	conn, err := net.Dial("tcp", broker.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	auth := base64.StdEncoding.EncodeToString([]byte("proxy:" + token))
	_, err = fmt.Fprintf(conn, "CONNECT 8.8.8.8:443 HTTP/1.1\r\nHost: 8.8.8.8:443\r\nProxy-Authorization: Basic %s\r\n\r\nrequest", auth)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT response = %v, error = %v", response, err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	reply, err := io.ReadAll(reader)
	if err != nil || string(reply) != "reply" {
		t.Fatalf("reply after FIN = %q, error = %v", reply, err)
	}
}
