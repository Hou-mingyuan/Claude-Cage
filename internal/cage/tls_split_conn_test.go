package cage

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingConn struct {
	net.Conn
	mu     sync.Mutex
	writes [][]byte
}

func (r *recordingConn) Write(p []byte) (int, error) {
	cp := append([]byte(nil), p...)
	r.mu.Lock()
	r.writes = append(r.writes, cp)
	r.mu.Unlock()
	return len(p), nil
}

func (r *recordingConn) Close() error { return nil }

func TestTLSClientHelloSplitConnSplitsLargeClientHello(t *testing.T) {
	rec := &recordingConn{}
	conn := newTLSClientHelloSplitConnWithDelay(rec, 1*time.Millisecond)

	// Build a synthetic 1522-byte TLS ClientHello record.
	payload := make([]byte, 1522)
	payload[0] = 0x16 // Handshake
	payload[1] = 0x03 // TLS 1.x
	payload[2] = 0x01
	payload[5] = 0x01 // ClientHello
	for i := 6; i < len(payload); i++ {
		payload[i] = byte(i)
	}

	n, err := conn.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("first Write = (%d, %v), want (%d, nil)", n, err, len(payload))
	}
	// A subsequent large write must NOT be split even if it starts with TLS bytes.
	second := append([]byte(nil), payload...)
	n2, err := conn.Write(second)
	if err != nil || n2 != len(second) {
		t.Fatalf("second Write = (%d, %v), want (%d, nil)", n2, err, len(second))
	}

	if len(rec.writes) != 3 {
		t.Fatalf("got %d underlying writes, want 3 (600 + 922 + 1522)", len(rec.writes))
	}
	if len(rec.writes[0]) != tlsClientHelloFirstChunk || len(rec.writes[1]) != len(payload)-tlsClientHelloFirstChunk {
		t.Fatalf("unexpected split sizes: %d, %d", len(rec.writes[0]), len(rec.writes[1]))
	}
	if !bytes.Equal(append(rec.writes[0], rec.writes[1]...), payload) {
		t.Fatal("reassembled ClientHello bytes differed from original")
	}
	if len(rec.writes[2]) != len(second) {
		t.Fatalf("subsequent write was split: got %d bytes", len(rec.writes[2]))
	}
}

func TestTLSClientHelloSplitConnLeavesSmallAndNonTLSUntouched(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"small ClientHello", func() []byte {
			b := make([]byte, 512)
			b[0], b[1], b[2], b[5] = 0x16, 0x03, 0x01, 0x01
			return b
		}()},
		{"large plain HTTP", bytes.Repeat([]byte("GET / HTTP/1.1\r\n"), 100)},
		{"large TLS application data", func() []byte {
			b := make([]byte, 1500)
			b[0], b[1], b[2], b[5] = 0x17, 0x03, 0x03, 0x01
			return b
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingConn{}
			conn := newTLSClientHelloSplitConnWithDelay(rec, 100*time.Millisecond)
			start := time.Now()
			n, err := conn.Write(tc.data)
			elapsed := time.Since(start)
			if err != nil || n != len(tc.data) {
				t.Fatalf("Write = (%d, %v)", n, err)
			}
			if len(rec.writes) != 1 || !bytes.Equal(rec.writes[0], tc.data) {
				t.Fatalf("expected single unmodified write, got %d writes", len(rec.writes))
			}
			if elapsed >= 50*time.Millisecond {
				t.Fatalf("non-matching write unexpectedly waited %v", elapsed)
			}
		})
	}
}

func selfSignedLoopbackTLS(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	return &tls.Config{Certificates: []tls.Certificate{cert}}, pool
}

// startPickyHTTPSServer simulates a proxy/middlebox that drops (closes with EOF)
// any TLS connection whose first read exceeds maxFirstRead bytes, while allowing
// split ClientHello writes to complete the full TLS 1.3 / ML-KEM handshake.
func startPickyHTTPSServer(t *testing.T, maxFirstRead int) (string, *x509.CertPool) {
	t.Helper()
	tlsCfg, pool := selfSignedLoopbackTLS(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "8.8.8.8\n")
	})}
	t.Cleanup(func() { _ = srv.Close() })

	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 4096)
				n, err := c.Read(buf)
				if err != nil || n > maxFirstRead {
					_ = c.Close()
					return
				}
				replay := &prefixConn{Conn: c, prefix: buf[:n]}
				tc := tls.Server(replay, tlsCfg)
				if err := tc.Handshake(); err != nil {
					_ = tc.Close()
					return
				}
				_ = srv.Serve(&singleConnListener{conn: tc})
			}(raw)
		}
	}()
	return "https://" + ln.Addr().String(), pool
}

type prefixConn struct {
	net.Conn
	prefix []byte
}

func (p *prefixConn) Read(b []byte) (int, error) {
	if len(p.prefix) > 0 {
		n := copy(b, p.prefix)
		p.prefix = p.prefix[n:]
		return n, nil
	}
	return p.Conn.Read(b)
}

type singleConnListener struct {
	conn net.Conn
	once sync.Once
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = l.conn })
	if c == nil {
		return nil, net.ErrClosed
	}
	return c, nil
}
func (l *singleConnListener) Close() error   { return nil }
func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

func TestTLSClientHelloSplitEndToEndThroughPickyServer(t *testing.T) {
	endpoint, pool := startPickyHTTPSServer(t, tlsClientHelloSplitThreshold)
	services := []exitProbeService{{id: "picky", name: "测试服务", endpoint: endpoint}}

	// Without split: Go's default TLS 1.3 ClientHello (with ML-KEM) exceeds 1000
	// bytes in a single write and is dropped with EOF, classified as ErrTLSHandshakeFailed.
	plainTransport := &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: pool, CurvePreferences: []tls.CurveID{tls.X25519MLKEM768, tls.X25519}},
		DisableKeepAlives: true,
	}
	defer plainTransport.CloseIdleConnections()
	ip, err := probeExitIPServices(context.Background(), &http.Client{Transport: plainTransport, Timeout: 3 * time.Second}, services, time.Minute, "")
	if ip != "" || !errors.Is(err, ErrTLSHandshakeFailed) || !strings.Contains(err.Error(), "TLS 握手失败") {
		t.Fatalf("expected ErrTLSHandshakeFailed without split, got ip=%q err=%v", ip, err)
	}

	// With split wrapper on the dialer: exact same TLS 1.3 ML-KEM ClientHello
	// succeeds without weakening cryptography or dropping post-quantum key shares.
	splitTransport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return newTLSClientHelloSplitConn(c), nil
		},
		TLSClientConfig:   &tls.Config{RootCAs: pool, CurvePreferences: []tls.CurveID{tls.X25519MLKEM768, tls.X25519}},
		DisableKeepAlives: true,
	}
	defer splitTransport.CloseIdleConnections()
	ip, err = probeExitIPServices(context.Background(), &http.Client{Transport: splitTransport, Timeout: 3 * time.Second}, services, time.Minute, "")
	if err != nil || ip != "8.8.8.8" {
		t.Fatalf("expected split ClientHello probe to succeed, got ip=%q err=%v", ip, err)
	}
}

func TestTLSClientHelloSplitLoadedFromWorkbenchPreferencesAndFingerprint(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	base := DefaultConfig()
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.TLSClientHelloSplit {
		t.Fatal("default TLSClientHelloSplit should be false")
	}
	defaultFP := loaded.Fingerprint()

	// Enable via sibling workbench.json; config.json remains free of unknown fields.
	if err := os.WriteFile(filepath.Join(dir, "workbench.json"), []byte(`{"tls_client_hello_split":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	loadedEnabled, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !loadedEnabled.TLSClientHelloSplit {
		t.Fatal("expected TLSClientHelloSplit=true from workbench.json")
	}
	if loadedEnabled.Fingerprint() == defaultFP {
		t.Fatal("expected Fingerprint to change when TLSClientHelloSplit is enabled")
	}
	// Ensure marshaling Config never emits tls_client_hello_split into config.json.
	marshaled, err := json.Marshal(loadedEnabled)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(marshaled), "tls_client_hello_split") {
		t.Fatalf("config.json must not contain tls_client_hello_split: %s", marshaled)
	}
}
