package managedproxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

func TestRealityAuthFailedMatchesCoreVariants(t *testing.T) {
	for _, line := range []string{
		"time=\"2026-11-26T10:00:00Z\" level=warning msg=\"[TCP] dial CAGE (match InUser/cage) 127.0.0.1:50000 --> ipv4.icanhazip.com:443 error: 203.0.113.5:443 connect error: REALITY authentication failed\"",
		"[ip] connect error: reality authentication failed",
		"REALITY: auth failed",
		"reality verification failed",
	} {
		if !RealityAuthFailed(line) {
			t.Fatalf("REALITY failure not recognized: %s", line)
		}
	}
	for _, line := range []string{"connect error: context deadline exceeded", "REALITY Authentication: true", "unreality authentication failed"} {
		if RealityAuthFailed(line) {
			t.Fatalf("unrelated line recognized as REALITY failure: %s", line)
		}
	}
}

func TestCoreLogStreamNamesOnlyConfirmedRealityHop(t *testing.T) {
	const relay, exit = "0123456789abcdef01234567", "89abcdef0123456789abcdef"
	for _, tc := range []struct {
		name, config, line, want string
	}{
		{"relay", "proxies: [{name: " + relay + ", type: vless, server: 198.51.100.1, port: 443}, {name: " + exit + ", type: vless, server: exit.example.com, port: 8443, dialer-proxy: " + relay + "}]\nproxy-groups: [{name: CAGE, type: select, proxies: [" + exit + "]}]\n",
			"[TCP] dial CAGE --> ipv4.icanhazip.com:443 error: exit.example.com:8443 connect error: 198.51.100.1:443 connect error: REALITY authentication failed", "relay"},
		{"exit", "proxies: [{name: " + relay + ", type: vless, server: 198.51.100.1, port: 443}, {name: " + exit + ", type: vless, server: exit.example.com, port: 8443, dialer-proxy: " + relay + "}]\nproxy-groups: [{name: CAGE, type: select, proxies: [" + exit + "]}]\n",
			"[TCP] dial CAGE --> ipv4.icanhazip.com:443 error: EXIT.example.com:8443 connect error: REALITY authentication failed", "exit"},
		{"single", "proxies: [{name: " + exit + ", type: vless, server: \"2001:db8::1\", port: 443}]\nproxy-groups: [{name: CAGE, type: select, proxies: [" + exit + "]}]\n",
			"[TCP] dial CAGE --> ipv4.icanhazip.com:443 error: [2001:db8::1]:443 connect error: REALITY authentication failed", "single"},
		{"shared endpoint", "proxies: [{name: " + relay + ", type: vless, server: 198.51.100.1, port: 443}, {name: " + exit + ", type: vless, server: 198.51.100.1, port: 443, dialer-proxy: " + relay + "}]\nproxy-groups: [{name: CAGE, type: select, proxies: [" + exit + "]}]\n",
			"error: 198.51.100.1:443 connect error: 198.51.100.1:443 connect error: REALITY authentication failed", ""},
		{"endpoint outside route", "proxies: [{name: " + exit + ", type: vless, server: 198.51.100.1, port: 443}]\nproxy-groups: [{name: CAGE, type: select, proxies: [" + exit + "]}]\n",
			"error: 192.0.2.9:443 connect error: REALITY authentication failed", ""},
		{"no endpoint", "proxies: [{name: " + exit + ", type: vless, server: 198.51.100.1, port: 443}]\nproxy-groups: [{name: CAGE, type: select, proxies: [" + exit + "]}]\n",
			"REALITY authentication failed", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := t.TempDir()
			if err := os.MkdirAll(Dir(profile), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(Dir(profile), "config.yaml"), []byte(tc.config), 0600); err != nil {
				t.Fatal(err)
			}
			store := diagnostics.New(profile)
			stream := CoreLogStream(store, profile, "warn")
			_, _ = stream.Write([]byte(tc.line + "\n"))
			_ = stream.Close()
			events, err := store.EventsSince(time.Now().Add(-time.Minute), "proxy")
			if err != nil {
				t.Fatal(err)
			}
			var hops []string
			failures := 0
			for _, event := range events {
				for _, private := range []string{"198.51.100.1", "exit.example.com", "2001:db8::1", "192.0.2.9", relay, exit} {
					if strings.Contains(strings.ToLower(event.Message), private) {
						t.Fatalf("stored core log kept %q: %s", private, event.Message)
					}
				}
				if hop, ok := strings.CutPrefix(event.Message, RealityHopMarker); ok {
					hops = append(hops, hop)
				} else if RealityAuthFailed(event.Message) {
					failures++
				}
			}
			if failures != 1 {
				t.Fatalf("redacted REALITY failure must stay recognizable, got %d in %+v", failures, events)
			}
			if tc.want == "" && len(hops) != 0 || tc.want != "" && (len(hops) != 1 || hops[0] != tc.want) {
				t.Fatalf("hop markers = %v, want %q", hops, tc.want)
			}
		})
	}
}
