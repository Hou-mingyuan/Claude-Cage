package managedproxy

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestLocalProxyAddresses(t *testing.T) {
	local := localServers{hostname: "workstation.example", addresses: map[netip.Addr]bool{netip.MustParseAddr("192.168.1.42"): true}}
	for _, addr := range []string{"127.17.2.3", "0177.0.0.1", "2130706433", "127.1", "0x7f000001", "::1", "[::1]", "::ffff:127.0.0.1", "0.0.0.0", "::", "LOCALHOST.", "proxy.localhost", "workstation", "WORKSTATION.local.", "workstation.example", "192.168.1.42"} {
		if !local.contains(addr) {
			t.Errorf("accepted local node %s", addr)
		}
	}
	for _, addr := range []string{"192.168.1.41", "203.0.113.1", "localhost.example.org"} {
		if local.contains(addr) {
			t.Errorf("blocked remote node %s", addr)
		}
	}
}

func TestLegacyLocalNodeMigration(t *testing.T) {
	profile := t.TempDir()
	s, err := Load(profile)
	if err != nil {
		t.Fatal(err)
	}
	const data = "proxies: [{name: entry, type: socks5, server: entry.example, port: 1}, {name: exit, type: http, server: exit.example, port: 2, dialer-proxy: entry}, {name: replacement, type: http, server: replacement.example, port: 3}]"
	if err := s.Import("old", "", []byte(data), "old", 0, ""); err != nil {
		t.Fatal(err)
	}
	nodes := s.Sources[0].Nodes
	entry, exit, replacement := nodes[0].ID, nodes[1].ID, nodes[2].ID
	nodes[0].Config["server"] = "127.8.7.6"
	s.Selected = exit
	s.Revision = "previous"
	if err := s.Save(profile); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(profile)
	if err != nil {
		t.Fatal(err)
	}
	route, err := loaded.Route()
	if !errors.Is(err, ErrLocalProxyNode) || len(route) != 2 || route[0] != entry || route[1] != exit {
		t.Fatalf("saved route unavailable for migration: %v, %v", route, err)
	}
	if _, err := Applied(profile, s.Endpoint(), s.Revision); !errors.Is(err, ErrLocalProxyNode) {
		t.Errorf("applied legacy route: %v", err)
	}
	if _, err := loaded.Runtime(profile); !errors.Is(err, ErrLocalProxyNode) {
		t.Errorf("generated legacy runtime: %v", err)
	}
	if _, err := loaded.NodeEndpoint(exit); !errors.Is(err, ErrLocalProxyNode) {
		t.Errorf("account kept dependent route: %v", err)
	}
	loaded.Selected = replacement
	loaded.Chain = []string{entry, replacement}
	if _, err := loaded.Route(); !errors.Is(err, ErrLocalProxyNode) {
		t.Errorf("explicit local hop accepted: %v", err)
	}
	loaded.Chain = nil
	wire, err := loaded.Runtime(profile)
	if err != nil {
		t.Fatal(err)
	}
	var core struct {
		Proxies               []struct{ Name string }
		Authentication, Rules []string
	}
	if err := yaml.Unmarshal(wire, &core); err != nil {
		t.Fatal(err)
	}
	if len(core.Proxies) != 1 || core.Proxies[0].Name != replacement || len(core.Authentication) != 2 || strings.Contains(strings.Join(core.Rules, " "), entry) || strings.Contains(strings.Join(core.Rules, " "), exit) {
		t.Fatal("blocked node or credentials in replacement runtime")
	}
	if _, found := loaded.Find(entry); !found {
		t.Fatal("original node removed")
	}
}

func TestSavedCoreCannotResumeLocalNode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("interface-name: eth0\nproxies: [{name: previous, server: localhost}]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := BoundInterface(path); !errors.Is(err, ErrLocalProxyNode) {
		t.Fatalf("old runtime accepted: %v", err)
	}
}

func TestImportRejectsHostInterfaceAddress(t *testing.T) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range addrs {
		p, err := netip.ParsePrefix(addr.String())
		if err != nil || !p.Addr().Is4() || p.Addr().IsLoopback() {
			continue
		}
		_, err = Parse([]byte("proxies: [{name: local, type: socks5, server: "+p.Addr().String()+", port: 10808}]"), "test")
		if err == nil || !strings.Contains(err.Error(), ErrLocalProxyNode.Error()) {
			t.Fatalf("host interface address accepted: %v", err)
		}
		return
	}
	t.Skip("no non-loopback IPv4 interface")
}

func TestAppliedRejectsStaleRuntimeWithLocalAccountNode(t *testing.T) {
	profile := t.TempDir()
	s, err := Load(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Import("remote", "", []byte("proxies: [{name: remote, type: socks5, server: remote.example, port: 1080}]"), "remote", 0, ""); err != nil {
		t.Fatal(err)
	}
	s.Selected = s.Sources[0].Nodes[0].ID
	s.Revision = "current"
	if err := s.Save(profile); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(Dir(profile), "config.yaml")
	if err := os.WriteFile(path, []byte("interface-name: eth0\nproxies: [{server: localhost}]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Applied(profile, s.Endpoint(), s.Revision); !errors.Is(err, ErrLocalProxyNode) {
		t.Fatalf("accepted previous runtime: %v", err)
	}
}
