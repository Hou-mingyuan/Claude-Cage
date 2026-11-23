package managedproxy

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"go.yaml.in/yaml/v3"
)

func mockHostNetwork(t *testing.T, ifaces []hostInterface, route func(netip.Addr) (netip.Addr, bool)) {
	t.Helper()
	oldIfaces, oldProbe := listHostInterfaces, probeRouteSourceIP
	t.Cleanup(func() {
		listHostInterfaces = oldIfaces
		probeRouteSourceIP = oldProbe
	})
	listHostInterfaces = func() ([]hostInterface, error) { return ifaces, nil }
	probeRouteSourceIP = route
}

func proxyMapByName(t *testing.T, data []byte) (map[string]any, map[string]map[string]any) {
	t.Helper()
	var cfg map[string]any
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	if list, ok := cfg["proxies"].([]any); ok {
		for _, item := range list {
			node := item.(map[string]any)
			name, _ := node["name"].(string)
			out[name] = node
		}
	}
	return cfg, out
}

func TestBindInterfaceRoutesTailscaleAndPreservesPhysicalLoopAvoidance(t *testing.T) {
	// Windows host with WLAN 5 (physical), Tailscale Wintun, and a host Clash
	// TUN ("Meta") hijacking the default route.
	windowsIfaces := []hostInterface{
		{name: "WLAN 5", prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.1.50/24")}},
		{name: "Tailscale", prefixes: []netip.Prefix{
			netip.MustParsePrefix("100.100.1.5/32"),
			netip.MustParsePrefix("fd7a:115c:a1e0::5/128"),
		}},
		{name: "Meta", prefixes: []netip.Prefix{netip.MustParsePrefix("198.18.0.1/16")}},
	}
	mockHostNetwork(t, windowsIfaces, func(dst netip.Addr) (netip.Addr, bool) {
		if tailscaleIPv4Prefix.Contains(dst) {
			return netip.MustParseAddr("100.100.1.5"), true
		}
		if tailscaleIPv6Prefix.Contains(dst) {
			return netip.MustParseAddr("fd7a:115c:a1e0::5"), true
		}
		// Simulate Clash TUN capturing every other destination via 0.0.0.0/0.
		return netip.MustParseAddr("198.18.0.1"), true
	})

	input := []byte(`proxies:
  - {name: ts-socks, type: socks5, server: 100.88.2.4, port: 1080}
  - {name: ts-v6, type: socks5, server: "[fd7a:115c:a1e0::88]", port: 1080}
  - {name: public-ip, type: socks5, server: 203.0.113.10, port: 1080}
  - {name: public-host, type: http, server: us-exit.example.com, port: 443}
  - {name: fake-ip, type: socks5, server: 198.18.0.5, port: 1080}
`)
	bound, err := BindInterface(input, "WLAN 5")
	if err != nil {
		t.Fatal(err)
	}
	cfg, nodes := proxyMapByName(t, bound)
	if cfg["interface-name"] != "WLAN 5" {
		t.Fatalf("top-level interface-name = %v, want WLAN 5", cfg["interface-name"])
	}
	if nodes["ts-socks"]["interface-name"] != "Tailscale" {
		t.Fatalf("ts-socks interface-name = %v, want Tailscale", nodes["ts-socks"]["interface-name"])
	}
	if nodes["ts-v6"]["interface-name"] != "Tailscale" {
		t.Fatalf("ts-v6 interface-name = %v, want Tailscale", nodes["ts-v6"]["interface-name"])
	}
	for _, stayed := range []string{"public-ip", "public-host", "fake-ip"} {
		if got, has := nodes[stayed]["interface-name"]; has {
			t.Fatalf("%s unexpectedly bound to %v instead of inheriting physical WLAN 5", stayed, got)
		}
	}

	// Without Tailscale (only host Clash TUN capturing 0/0), a 100.64.0.0/10
	// server must NOT be bound to the Clash TUN ("Meta").
	noTailscale := []hostInterface{
		{name: "WLAN 5", prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.1.50/24")}},
		{name: "Meta", prefixes: []netip.Prefix{netip.MustParsePrefix("198.18.0.1/16")}},
	}
	mockHostNetwork(t, noTailscale, func(dst netip.Addr) (netip.Addr, bool) {
		return netip.MustParseAddr("198.18.0.1"), true
	})
	rebound, err := BindInterface(bound, "WLAN 5")
	if err != nil {
		t.Fatal(err)
	}
	_, nodesAfter := proxyMapByName(t, rebound)
	if nodesAfter["ts-socks"]["interface-name"] != "WLAN 5" {
		t.Fatalf("disconnected Tailscale node bound to %v, want physical fallback WLAN 5", nodesAfter["ts-socks"]["interface-name"])
	}
}

func TestBindInterfaceCrossPlatformMacOSAndLinuxSubnets(t *testing.T) {
	// macOS: Wi-Fi en0 + Tailscale utun4.
	macIfaces := []hostInterface{
		{name: "en0", prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.1.20/24")}},
		{name: "utun4", prefixes: []netip.Prefix{netip.MustParsePrefix("100.64.9.10/32")}},
	}
	mockHostNetwork(t, macIfaces, func(dst netip.Addr) (netip.Addr, bool) {
		if tailscaleIPv4Prefix.Contains(dst) {
			return netip.MustParseAddr("100.64.9.10"), true
		}
		return netip.MustParseAddr("192.168.1.20"), true
	})
	macBound, err := BindInterface([]byte("proxies:\n  - {name: ts, type: socks5, server: 100.64.9.20, port: 1080}\n"), "en0")
	if err != nil {
		t.Fatal(err)
	}
	cfg, nodes := proxyMapByName(t, macBound)
	if cfg["interface-name"] != "en0" || nodes["ts"]["interface-name"] != "utun4" {
		t.Fatalf("macOS binding unexpected: top=%v node=%v", cfg["interface-name"], nodes["ts"]["interface-name"])
	}

	// Linux: eth0 physical, tailscale0 (policy table 52), wg0 directly attached
	// 10.8.0.0/24 subnet, and tun0 (172.19.0.1/30) default-route TUN.
	linuxIfaces := []hostInterface{
		{name: "eth0", prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.1.30/24")}},
		{name: "tailscale0", prefixes: []netip.Prefix{netip.MustParsePrefix("100.100.2.3/32")}},
		{name: "wg0", prefixes: []netip.Prefix{netip.MustParsePrefix("10.8.0.2/24")}},
		{name: "tun0", prefixes: []netip.Prefix{netip.MustParsePrefix("172.19.0.1/30")}},
	}
	mockHostNetwork(t, linuxIfaces, func(dst netip.Addr) (netip.Addr, bool) {
		switch {
		case tailscaleIPv4Prefix.Contains(dst):
			return netip.MustParseAddr("100.100.2.3"), true
		case netip.MustParsePrefix("10.8.0.0/24").Contains(dst):
			return netip.MustParseAddr("10.8.0.2"), true
		default:
			// Simulate tun0 capturing 0.0.0.0/0 including unrelated RFC 1918 10.20.30.40.
			return netip.MustParseAddr("172.19.0.1"), true
		}
	})
	linuxInput := []byte(`proxies:
  - {name: ts, type: socks5, server: 100.100.2.99, port: 1080}
  - {name: wg-peer, type: socks5, server: 10.8.0.1, port: 1080}
  - {name: tun-captured-private, type: socks5, server: 10.20.30.40, port: 1080}
  - {name: same-physical-lan, type: socks5, server: 192.168.1.1, port: 1080}
`)
	linuxBound, err := BindInterface(linuxInput, "eth0")
	if err != nil {
		t.Fatal(err)
	}
	cfg, nodes = proxyMapByName(t, linuxBound)
	if cfg["interface-name"] != "eth0" {
		t.Fatalf("linux top-level = %v, want eth0", cfg["interface-name"])
	}
	if nodes["ts"]["interface-name"] != "tailscale0" {
		t.Fatalf("linux tailscale node = %v, want tailscale0", nodes["ts"]["interface-name"])
	}
	if nodes["wg-peer"]["interface-name"] != "wg0" {
		t.Fatalf("linux wg subnet node = %v, want wg0", nodes["wg-peer"]["interface-name"])
	}
	if got, has := nodes["tun-captured-private"]["interface-name"]; has {
		t.Fatalf("unrelated private IP captured by default TUN was bound to %v", got)
	}
	if got, has := nodes["same-physical-lan"]["interface-name"]; has {
		t.Fatalf("physical LAN node got unnecessary node override %v", got)
	}
}

func TestBindInterfaceDialerProxyChainsAndAccountRouteClones(t *testing.T) {
	ifaces := []hostInterface{
		{name: "WLAN 5", prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.1.50/24")}},
		{name: "Tailscale", prefixes: []netip.Prefix{netip.MustParsePrefix("100.100.1.5/32")}},
	}
	mockHostNetwork(t, ifaces, func(dst netip.Addr) (netip.Addr, bool) {
		if tailscaleIPv4Prefix.Contains(dst) {
			return netip.MustParseAddr("100.100.1.5"), true
		}
		return netip.MustParseAddr("192.168.1.50"), true
	})

	profile := t.TempDir()
	s, err := Load(profile)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Import("mixed", "", []byte(`proxies:
  - {name: pub-entry, type: socks5, server: 203.0.113.1, port: 1080}
  - {name: ts-node, type: socks5, server: 100.88.2.4, port: 1080}
  - {name: pub-exit, type: http, server: 203.0.113.2, port: 443}
`), "mixed", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	pubEntry := s.Sources[0].Nodes[0].ID
	tsNode := s.Sources[0].Nodes[1].ID
	pubExit := s.Sources[0].Nodes[2].ID

	// Case 1: Explicit chain pub-entry -> ts-node.
	// The main ts-node entry has dialer-proxy: pub-entry (must NOT bind to host
	// Tailscale), while its direct account-route clone "route-"+tsNode has no
	// dialer-proxy (MUST bind to host Tailscale).
	s.Chain = []string{pubEntry, tsNode}
	s.Selected = tsNode
	raw, err := s.Runtime(profile)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := BindInterface(raw, "WLAN 5")
	if err != nil {
		t.Fatal(err)
	}
	_, nodes := proxyMapByName(t, bound)
	if got, has := nodes[tsNode]["interface-name"]; has {
		t.Fatalf("chained remote Tailscale hop should not bind to host Tailscale: %v", got)
	}
	if nodes["route-"+tsNode]["interface-name"] != "Tailscale" {
		t.Fatalf("direct account-route clone = %v, want Tailscale", nodes["route-"+tsNode]["interface-name"])
	}

	// Case 2: Explicit chain ts-node -> pub-exit.
	// Direct entry ts-node binds to Tailscale; chained pub-exit has no override.
	s.Chain = []string{tsNode, pubExit}
	s.Selected = pubExit
	raw, err = s.Runtime(profile)
	if err != nil {
		t.Fatal(err)
	}
	bound, err = BindInterface(raw, "WLAN 5")
	if err != nil {
		t.Fatal(err)
	}
	_, nodes = proxyMapByName(t, bound)
	if nodes[tsNode]["interface-name"] != "Tailscale" {
		t.Fatalf("direct Tailscale chain entry = %v, want Tailscale", nodes[tsNode]["interface-name"])
	}
	if got, has := nodes[pubExit]["interface-name"]; has {
		t.Fatalf("chained public exit should not bind node interface-name: %v", got)
	}
}

func TestBindingMatchesDetectsTailscaleRouteChanges(t *testing.T) {
	tsUp := []hostInterface{
		{name: "WLAN 5", prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.1.50/24")}},
		{name: "Tailscale", prefixes: []netip.Prefix{netip.MustParsePrefix("100.100.1.5/32")}},
	}
	tsRoute := func(dst netip.Addr) (netip.Addr, bool) {
		if tailscaleIPv4Prefix.Contains(dst) {
			return netip.MustParseAddr("100.100.1.5"), true
		}
		return netip.MustParseAddr("192.168.1.50"), true
	}
	mockHostNetwork(t, tsUp, tsRoute)

	path := filepath.Join(t.TempDir(), "config.yaml")
	bound, err := BindInterface([]byte("proxies:\n  - {name: ts, type: socks5, server: 100.88.2.4, port: 1080}\n"), "WLAN 5")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bound, 0600); err != nil {
		t.Fatal(err)
	}
	if !BindingMatches(path, "WLAN 5") {
		t.Fatal("freshly bound Tailscale config did not match")
	}
	if BindingMatches(path, "Ethernet") {
		t.Fatal("physical interface change was not detected")
	}

	// Tailscale moves to a new interface name (e.g. utun4 -> utun5 or Tailscale 2).
	tsMoved := []hostInterface{
		{name: "WLAN 5", prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.1.50/24")}},
		{name: "Tailscale 2", prefixes: []netip.Prefix{netip.MustParsePrefix("100.100.1.5/32")}},
	}
	mockHostNetwork(t, tsMoved, tsRoute)
	if BindingMatches(path, "WLAN 5") {
		t.Fatal("Tailscale interface rename was not detected")
	}

	// Tailscale disconnects completely.
	tsDown := []hostInterface{
		{name: "WLAN 5", prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.1.50/24")}},
	}
	mockHostNetwork(t, tsDown, func(dst netip.Addr) (netip.Addr, bool) {
		return netip.MustParseAddr("192.168.1.50"), true
	})
	if BindingMatches(path, "WLAN 5") {
		t.Fatal("Tailscale disconnect was not detected")
	}

	// Rebinding while Tailscale is down matches until Tailscale comes back up.
	reboundDown, err := BindInterface(bound, "WLAN 5")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, reboundDown, 0600); err != nil {
		t.Fatal(err)
	}
	if !BindingMatches(path, "WLAN 5") {
		t.Fatal("rebound config while Tailscale down did not match")
	}
	mockHostNetwork(t, tsUp, tsRoute)
	if BindingMatches(path, "WLAN 5") {
		t.Fatal("Tailscale reconnect was not detected")
	}
}
