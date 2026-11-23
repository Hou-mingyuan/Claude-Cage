package managedproxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.yaml.in/yaml/v3"
)

func TestCoreSourceNodeDNSIsolation(t *testing.T) {
	if os.Getenv("CAGE_PROXY_INTEGRATION") != "1" {
		t.Skip("requires a test binary beside the bundled Mihomo")
	}
	// Only the isolated runtime substitutes local UDP resolvers. Production
	// import still accepts remote HTTPS DoH exclusively and Validate runs first.
	var mu sync.Mutex
	queries := map[string][]string{}
	resolver := func(label string) string {
		t.Helper()
		pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
			reply := new(dns.Msg)
			reply.SetReply(q)
			for _, question := range q.Question {
				mu.Lock()
				queries[label] = append(queries[label], question.Name)
				mu.Unlock()
				if question.Qtype == dns.TypeA {
					reply.Answer = append(reply.Answer, &dns.A{Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30}, A: net.ParseIP("127.0.0.1")})
				}
			}
			_ = w.WriteMsg(reply)
		})}
		ready := make(chan struct{})
		server.NotifyStartedFunc = func() { close(ready) }
		go func() { _ = server.ActivateAndServe() }()
		<-ready
		t.Cleanup(func() { _ = server.Shutdown() })
		return "udp://" + pc.LocalAddr().String()
	}
	defaultDNS, firstDNS, secondDNS := resolver("default"), resolver("first"), resolver("second")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(target.Close)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != strings.TrimPrefix(target.URL, "http://") {
			t.Error("unexpected proxy request")
			w.WriteHeader(403)
			return
		}
		remote, err := net.DialTimeout("tcp4", r.Host, time.Second)
		if err != nil {
			t.Error(err)
			return
		}
		defer remote.Close()
		client, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		_, _ = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() { _, _ = io.Copy(remote, buffered) }()
		_, _ = io.Copy(client, remote)
	}))
	t.Cleanup(upstream.Close)
	profile := t.TempDir()
	s, _ := Load(profile)
	for i, host := range []string{"first.example.test", "second.example.test", "ordinary.example.test"} {
		input := sourceDNSInput(host)
		if i < 2 {
			input = sourceDNSInput(host, fmt.Sprintf("https://resolver-%d.example.test/dns-query", i))
		}
		input = strings.Replace(input, "port: 8080", fmt.Sprintf("port: %d", upstream.Listener.Addr().(*net.TCPAddr).Port), 1)
		if err := s.Import(fmt.Sprint(i), "", []byte(input), fmt.Sprint(i), 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	s.Selected = s.Sources[0].Nodes[0].ID
	if err := Validate(context.Background(), profile, s); err != nil {
		t.Fatal(err)
	}
	// Round-trip persisted sources before generating the restarted core's config.
	if err := s.Save(profile); err != nil {
		t.Fatal(err)
	}
	s, _ = Load(profile)
	raw, err := s.Runtime(profile)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err = yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	d := cfg["dns"].(map[string]any)
	d["default-nameserver"] = []string{defaultDNS}
	d["proxy-server-nameserver"] = []string{defaultDNS}
	d["nameserver"] = []string{defaultDNS}
	policy := d["proxy-server-nameserver-policy"].(map[string]any)
	policy["first.example.test"] = []string{firstDNS}
	policy["second.example.test"] = []string{secondDNS}
	cfg["mixed-port"] = 0
	raw, _ = yaml.Marshal(cfg)
	config := filepath.Join(Dir(profile), "config.yaml")
	if err = Write(config, raw); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(Socket(profile)), 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(Socket(profile))) })
	core, err := CorePath()
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(profile, "core.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	cmd := exec.Command(core, "-d", Dir(profile), "-f", config)
	cmd.Env = coreEnvironment(Dir(profile))
	cmd.Stdout, cmd.Stderr = log, log
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for {
		var version map[string]any
		if err = API(ctx, profile, "GET", "/version", nil, &version); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("core did not start", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	for _, src := range s.Sources {
		node := src.Nodes[0]
		var result map[string]any
		if err = API(ctx, profile, "GET", "/proxies/"+node.ID+"/delay?timeout=3000&url="+url.QueryEscape(target.URL), nil, &result); err != nil {
			t.Fatal("node resolver did not reach its source proxy", src.ID, err)
		}
	}
	// Even a business lookup for an identical domain must use the main resolver.
	if err = API(ctx, profile, "GET", "/dns/query?name=first.example.test&type=A", nil, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for label, allowed := range map[string]string{"first": "first.example.test.", "second": "second.example.test."} {
		if len(queries[label]) == 0 {
			t.Fatal("source DNS was unused", label)
		}
		for _, query := range queries[label] {
			if query != allowed {
				t.Fatal("source DNS received another source or business domain", label, query)
			}
		}
	}
	for _, host := range []string{"ordinary.example.test.", "first.example.test."} {
		found := false
		for _, query := range queries["default"] {
			found = found || query == host
		}
		if !found {
			t.Fatal("default/business resolver was bypassed", host)
		}
	}
}
