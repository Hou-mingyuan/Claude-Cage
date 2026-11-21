package cage

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func dohServer(t *testing.T, fail *atomic.Bool, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		body, _ := io.ReadAll(r.Body)
		q := new(dns.Msg)
		if q.Unpack(body) != nil {
			t.Error("invalid DNS request")
			return
		}
		body, _ = new(dns.Msg).SetReply(q).Pack()
		w.Write(body)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestDNSFailsOverToBackupEndpointThroughProxy(t *testing.T) {
	var primaryDown, backupDown atomic.Bool
	var primaryHits, backupHits atomic.Int32
	primaryDown.Store(true)
	primary := dohServer(t, &primaryDown, &primaryHits)
	backup := dohServer(t, &backupDown, &backupHits)
	p, connects := testProxy(t)
	defer p.Close()
	c := DefaultConfig()
	c.ProxyURL, c.DNSURL = p.URL, primary.URL
	c.DNS = NewDNSOptions([]string{backup.URL}, nil, false)
	e, err := NewEgress(c)
	if err != nil {
		t.Fatal(err)
	}
	defer e.client.CloseIdleConnections()
	roots := primary.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	pool := roots.Clone()
	pool.AddCert(backup.Certificate())
	e.client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool}
	q := new(dns.Msg).SetQuestion("example.com.", dns.TypeA)
	if _, err := e.ExchangeDNS(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	if primaryHits.Load() != 1 || backupHits.Load() != 1 || connects.Load() == 0 {
		t.Fatalf("failover order wrong: primary=%d backup=%d connects=%d", primaryHits.Load(), backupHits.Load(), connects.Load())
	}
	// The endpoint that answered is tried first next time.
	if _, err := e.ExchangeDNS(t.Context(), q); err != nil || primaryHits.Load() != 1 || backupHits.Load() != 2 {
		t.Fatalf("answering endpoint not preferred: %v primary=%d backup=%d", err, primaryHits.Load(), backupHits.Load())
	}
	backupDown.Store(true)
	_, err = e.ExchangeDNS(t.Context(), q)
	if err == nil || !strings.Contains(err.Error(), "设置 → 网络与代理 → 加密 DNS") || !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("all endpoints down must point to the settings: %v", err)
	}
	if strings.Contains(err.Error(), "/dns-query") || strings.Contains(err.Error(), primary.URL) {
		t.Fatalf("error leaked an endpoint path: %v", err)
	}
}

func TestDNSEndpointsDefaultsAndOptIn(t *testing.T) {
	c := DefaultConfig()
	if got := c.DNSEndpoints(); !reflect.DeepEqual(got, []string{"https://1.1.1.1/dns-query"}) {
		t.Fatalf("default must stay a single provider: %v", got)
	}
	if got := c.BootstrapEndpoints(); !reflect.DeepEqual(got, DefaultBootstrapDNS) {
		t.Fatalf("bootstrap default changed: %v", got)
	}
	c.DNS = NewDNSOptions([]string{" https://dns.example/dns-query ", "https://1.1.1.1/dns-query", ""}, []string{"https://9.9.9.9/dns-query"}, true)
	if got := c.DNSEndpoints(); !reflect.DeepEqual(got, []string{"https://1.1.1.1/dns-query", "https://dns.example/dns-query", PublicDNSFallback}) {
		t.Fatalf("endpoint order: %v", got)
	}
	if got := c.BootstrapEndpoints(); !reflect.DeepEqual(got, []string{"https://9.9.9.9/dns-query"}) {
		t.Fatalf("bootstrap override: %v", got)
	}
	base := DefaultConfig()
	if base.Fingerprint() == c.Fingerprint() {
		t.Fatal("changing DNS options must change Config.Fingerprint")
	}
	for _, bad := range []DNSOptions{
		NewDNSOptions([]string{"http://dns.example/dns-query"}, nil, false),
		NewDNSOptions([]string{"https://user@dns.example/dns-query"}, nil, false),
		NewDNSOptions(nil, []string{"https://dns.example/dns-query"}, false),
		NewDNSOptions([]string{"https://a/", "https://b/", "https://c/", "https://d/", "https://e/"}, nil, false),
	} {
		c.DNS = bad
		if c.Validate() == nil {
			t.Fatalf("invalid options accepted: %+v", bad)
		}
	}
}

func TestDNSOptionsFileKeepsMainConfigReadableByOlderVersions(t *testing.T) {
	dir := t.TempDir()
	if !strings.HasPrefix(dir, os.TempDir()) {
		t.Fatalf("test directory %q outside temp", dir)
	}
	path := filepath.Join(dir, "config.json")
	c := DefaultConfig()
	b, _ := json.Marshal(c)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	o := NewDNSOptions([]string{"https://dns.example/dns-query"}, []string{"https://223.5.5.5/dns-query"}, true)
	if err := SaveDNSOptions(path, o); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil || loaded.DNS != o {
		t.Fatalf("options not loaded: %+v %v", loaded.DNS, err)
	}
	main, _ := os.ReadFile(path)
	if strings.Contains(string(main), "dns.example") {
		t.Fatal("options leaked into the strict main configuration")
	}
	if err := SaveDNSOptions(path, DNSOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, DNSOptionsFile)); !os.IsNotExist(err) {
		t.Fatal("default options must not leave a file")
	}
	if err := os.WriteFile(filepath.Join(dir, DNSOptionsFile), []byte(`{"unknown":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("malformed options accepted")
	}
}

// One transient primary failure must not keep every later query on the
// backup (which may be a second provider the user opted into).
func TestDNSRetriesPrimaryAfterInterval(t *testing.T) {
	var primaryDown, backupDown atomic.Bool
	var primaryHits, backupHits atomic.Int32
	primaryDown.Store(true)
	primary := dohServer(t, &primaryDown, &primaryHits)
	backup := dohServer(t, &backupDown, &backupHits)
	p, _ := testProxy(t)
	defer p.Close()
	c := DefaultConfig()
	c.ProxyURL, c.DNSURL = p.URL, primary.URL
	c.DNS = NewDNSOptions([]string{backup.URL}, nil, false)
	e, err := NewEgress(c)
	if err != nil {
		t.Fatal(err)
	}
	defer e.client.CloseIdleConnections()
	pool := primary.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs.Clone()
	pool.AddCert(backup.Certificate())
	e.client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool}
	q := new(dns.Msg).SetQuestion("example.com.", dns.TypeA)
	query := func(wantPrimary, wantBackup int32) {
		t.Helper()
		if _, err := e.ExchangeDNS(t.Context(), q); err != nil {
			t.Fatal(err)
		}
		if primaryHits.Load() != wantPrimary || backupHits.Load() != wantBackup {
			t.Fatalf("primary=%d backup=%d, want %d/%d", primaryHits.Load(), backupHits.Load(), wantPrimary, wantBackup)
		}
	}
	retryDue := func() { e.dnsPrimaryRetry.Store(time.Now().Add(-time.Second).UnixNano()) }
	query(1, 1) // primary fails once, backup answers
	primaryDown.Store(false)
	query(1, 2) // within the interval the backup stays first
	if due := time.Until(time.Unix(0, e.dnsPrimaryRetry.Load())); due < dnsPrimaryRetryInterval-time.Minute || due > dnsPrimaryRetryInterval {
		t.Fatalf("primary retry scheduled in %v, want about %v", due, dnsPrimaryRetryInterval)
	}
	primaryDown.Store(true)
	retryDue()
	query(2, 3) // retry: primary still down, one attempt, then backup
	query(2, 4) // and not retried again before the next interval
	primaryDown.Store(false)
	retryDue()
	query(3, 4) // primary back: it answers and is preferred again
	query(4, 4)
}
