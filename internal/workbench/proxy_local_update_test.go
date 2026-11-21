package workbench

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
)

func localUpdateServer(t *testing.T) (*Server, string) {
	t.Helper()
	s := testServer(t)
	if !strings.HasPrefix(s.profile, os.TempDir()) {
		t.Fatalf("profile %q is outside the test temp dir", s.profile)
	}
	p, _ := managedproxy.Load(s.profile)
	if err := p.Import("本地", "", []byte(`proxies:
  - {name: A, type: socks5, server: 203.0.113.10, port: 1081, username: u, password: keep-secret}
  - {name: B, type: socks5, server: 203.0.113.10, port: 1082}
`), "local", 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := p.Import("订阅", "https://sub.example.com/x", []byte(`proxies: [{name: R, type: socks5, server: 203.0.113.10, port: 1090}]`), "remote", 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := p.Save(s.profile); err != nil {
		t.Fatal(err)
	}
	validate, start := validateProxyCore, startProxyCore
	t.Cleanup(func() { validateProxyCore, startProxyCore = validate, start })
	validateProxyCore = func(_ context.Context, profile string, p managedproxy.Store) error {
		_, err := p.Runtime(profile)
		return err
	}
	startProxyCore = func(context.Context, string, managedproxy.Store) error { return nil }
	a := p.Sources[1].Nodes[0].ID
	if err := s.changeProxy("select", proxyInput{NodeID: a}, nil); err != nil {
		t.Fatal(err)
	}
	c, _ := loadConfig(s.configPath)
	c.ExitPolicy.Mode = "static"
	c.ExitPolicy = c.ExitPolicy.BindMissing(cage.ExitLocation{IP: "1.1.1.1", CountryCode: "US"})
	if err := s.saveExitPolicyConfig(c); err != nil {
		t.Fatal(err)
	}
	return s, a
}

func nodeConfig(t *testing.T, s *Server, source, name string) map[string]any {
	t.Helper()
	p, _ := managedproxy.Load(s.profile)
	for _, src := range p.Sources {
		if src.ID != source {
			continue
		}
		for _, n := range src.Nodes {
			if n.Name == name {
				return n.Config
			}
		}
	}
	return nil
}

func TestLocalSourceRefreshFromNewYAMLKeepsSelectionAndBinding(t *testing.T) {
	s, a := localUpdateServer(t)
	err := s.changeProxy("refresh", proxyInput{ID: "local", Content: `proxies:
  - {name: A, type: socks5, server: 203.0.113.20, port: 2081, username: u, password: new-secret}
  - {name: C, type: http, server: 203.0.113.30, port: 3128}
`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := managedproxy.Load(s.profile)
	cfg, _ := loadConfig(s.configPath)
	if p.Selected != a || cfg.ExitPolicy.IP != "1.1.1.1" {
		t.Fatalf("update lost the selection or exit binding: selected=%s policy=%+v", p.Selected, cfg.ExitPolicy)
	}
	if got := nodeConfig(t, s, "local", "A"); got["server"] != "203.0.113.20" || fmt.Sprint(got["port"]) != "2081" || got["password"] != "new-secret" {
		t.Fatalf("node A was not updated: %v", got)
	}
	if nodeConfig(t, s, "local", "B") != nil || nodeConfig(t, s, "local", "C") == nil {
		t.Fatal("node list was not replaced")
	}
	if p.Sources[1].Name != "本地" || p.Sources[1].URL != "" {
		t.Fatal("update changed the source identity")
	}
	// Removing the applied exit keeps the saved source untouched.
	if err := s.changeProxy("refresh", proxyInput{ID: "local", Content: `proxies: [{name: C, type: http, server: 203.0.113.30, port: 3128}]`}, nil); err == nil || !strings.Contains(err.Error(), "原配置已保留") {
		t.Fatalf("removing the current exit was accepted: %v", err)
	}
	if nodeConfig(t, s, "local", "A") == nil {
		t.Fatal("failed update changed the source")
	}
	if err := s.changeProxy("refresh", proxyInput{ID: "local"}, nil); err == nil {
		t.Fatal("local refresh without content accepted")
	}
	if err := s.changeProxy("refresh", proxyInput{ID: "remote", Content: "proxies: []"}, nil); err == nil {
		t.Fatal("pasted content replaced a subscription")
	}
}

func TestEditNodeAddressKeepsCredentialsAndIdentity(t *testing.T) {
	s, a := localUpdateServer(t)
	if err := s.changeProxy("edit-node", proxyInput{ID: "local", NodeID: a, Server: " node.example.com ", ServerPort: 4443}, nil); err != nil {
		t.Fatal(err)
	}
	got := nodeConfig(t, s, "local", "A")
	if got["server"] != "node.example.com" || fmt.Sprint(got["port"]) != "4443" || got["password"] != "keep-secret" || got["username"] != "u" {
		t.Fatalf("edit changed more than the address: %v", got)
	}
	p, _ := managedproxy.Load(s.profile)
	cfg, _ := loadConfig(s.configPath)
	if p.Selected != a || cfg.ExitPolicy.IP != "1.1.1.1" {
		t.Fatal("edit lost selection or exit binding; the next exit check decides whether the new address still matches")
	}
	if err := s.changeProxy("edit-node", proxyInput{ID: "local", NodeID: a, Server: "[2001:db8::1]", ServerPort: 443}, nil); err != nil {
		t.Fatal(err)
	}
	if got := nodeConfig(t, s, "local", "A"); got["server"] != "2001:db8::1" {
		t.Fatalf("IPv6 literal not stored bare: %v", got["server"])
	}
	remote := ""
	for _, src := range p.Sources {
		if src.ID == "remote" {
			remote = src.Nodes[0].ID
		}
	}
	for _, bad := range []proxyInput{
		{ID: "remote", NodeID: remote, Server: "198.51.100.1", ServerPort: 1},
		{ID: "local", NodeID: a, Server: "bad host", ServerPort: 1},
		{ID: "local", NodeID: a, Server: "user@host", ServerPort: 1},
		{ID: "local", NodeID: a, Server: "", ServerPort: 1},
		{ID: "local", NodeID: a, Server: "198.51.100.1", ServerPort: 0},
		{ID: "local", NodeID: a, Server: "198.51.100.1", ServerPort: 70000},
		{ID: "local", NodeID: "missing", Server: "198.51.100.1", ServerPort: 1},
	} {
		if err := s.changeProxy("edit-node", bad, nil); err == nil {
			t.Fatalf("invalid edit accepted: %+v", bad)
		}
	}
	if got := nodeConfig(t, s, "local", "A"); got["server"] != "2001:db8::1" || fmt.Sprint(got["port"]) != "443" {
		t.Fatal("rejected edits changed the node")
	}
}
