package managedproxy

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestPhysicalInterfaceFollowsHardwareRouteAndSkipsVirtualNICs(t *testing.T) {
	hardware := "Hardware Port: Wi-Fi\nDevice: en0\nHardware Port: USB Ethernet\nDevice: en7\nHardware Port: VPN\nDevice: utun7\nHardware Port: VM Network\nDevice: vmnet1\nHardware Port: Bridge\nDevice: bridge0\nHardware Port: VM Link\nDevice: vmen0\nHardware Port: Lima\nDevice: lima0\nHardware Port: AWDL\nDevice: awdl0\n"
	nwi := "IPv4 network interface information\n utun7 : flags : 0x7\n reach : 0x00000002 (Reachable)\n vmnet1 : flags : 0x7\n reach : 0x00000002 (Reachable)\n en7 : flags : 0x7\n reach : 0x00000002 (Reachable)\n en0 : flags : 0x7\n reach : 0x00000002 (Reachable)\nIPv6 network interface information\n"
	for _, tc := range []struct {
		name, route, want string
		usable            map[string]bool
	}{
		{"wifi default", "interface: en0", "en0", map[string]bool{"en0": true, "en7": true, "utun7": true}},
		{"ethernet default", "interface: en7", "en7", map[string]bool{"en0": true, "en7": true}},
		{"TUN default uses physical priority", "interface: utun7", "en7", map[string]bool{"en0": true, "en7": true, "utun7": true}},
		{"VM link default uses physical priority", "interface: vmnet1", "en7", map[string]bool{"en0": true, "en7": true, "vmnet1": true}},
		{"unavailable ethernet", "interface: en7", "en0", map[string]bool{"en0": true}},
		{"only virtual interfaces", "interface: utun7", "", map[string]bool{"utun7": true, "vmnet1": true, "bridge0": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectPhysicalInterface(tc.route, hardware, nwi, tc.usable)
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	for _, name := range []string{"utun0", "utun12", "bridge0", "vmen0", "vmnet1", "tap0", "lima0", "awdl0", "llw0", "p2p0", "gif0", "stf0", "anpi0", "ap0"} {
		if !isVirtualMACInterface(name) {
			t.Fatalf("virtual interface %q was not filtered", name)
		}
	}
	for _, name := range []string{"en0", "en7", "eth0", "lo0"} {
		if isVirtualMACInterface(name) {
			t.Fatalf("physical interface %q was filtered", name)
		}
	}
	if _, err := selectPhysicalInterface("interface: en0", hardware, strings.ReplaceAll(nwi, "(Reachable)", "(Not Reachable)"), map[string]bool{"en0": true, "en7": true}); err == nil {
		t.Fatal("selected an unreachable interface")
	}
}

func TestPhysicalInterfaceRejectsAppleVirtualNetwork(t *testing.T) {
	hardware := "Hardware Port: VM Network\nDevice: vmenet0\nHardware Port: Wi-Fi\nDevice: en0\n"
	nwi := "IPv4 network interface information\n vmenet0 : flags : 0x7\n reach : 0x00000002 (Reachable)\n en0 : flags : 0x7\n reach : 0x00000002 (Reachable)\nIPv6 network interface information\n"
	for _, tc := range []struct {
		name   string
		usable map[string]bool
		want   string
	}{
		{"physical fallback", map[string]bool{"vmenet0": true, "en0": true}, "en0"},
		{"no physical fallback", map[string]bool{"vmenet0": true}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectPhysicalInterface("interface: vmenet0", hardware, nwi, tc.usable)
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestBindingPreservesProxyChainAndReplacesStaleNodeBinding(t *testing.T) {
	data := []byte("interface-name: en0\nrules: ['MATCH,exit']\nproxies:\n - {name: entry, type: socks5, server: 203.0.113.1, port: 1080, password: secret, interface-name: en0}\n - {name: exit, type: http, server: 203.0.113.2, port: 443, dialer-proxy: entry}\n")
	bound, err := BindInterface(data, "en7")
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := yaml.Unmarshal(bound, &config); err != nil {
		t.Fatal(err)
	}
	nodes := config["proxies"].([]any)
	if config["interface-name"] != "en7" || nodes[0].(map[string]any)["interface-name"] != "en7" || nodes[0].(map[string]any)["password"] != "secret" || nodes[1].(map[string]any)["dialer-proxy"] != "entry" || config["rules"].([]any)[0] != "MATCH,exit" {
		t.Fatal("binding changed the proxy chain or kept a stale interface")
	}
	if _, err := BindInterface(data, ""); err == nil {
		t.Fatal("unbound fallback accepted")
	}
}
