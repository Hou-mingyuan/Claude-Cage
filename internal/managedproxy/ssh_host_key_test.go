package managedproxy

import (
	"reflect"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestSSHHostKeyAlgorithmsFollowPinnedKeys(t *testing.T) {
	const ed25519 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFixtureKey"
	for _, tc := range []struct {
		name string
		node map[string]any
		want any
	}{
		{"pinned Ed25519 only", map[string]any{"type": "ssh", "host-key": []any{ed25519}}, []any{"ssh-ed25519"}},
		{"single key as string", map[string]any{"type": "ssh", "host-key": ed25519}, []any{"ssh-ed25519"}},
		{"RSA accepts SHA-2 and SHA-1 signatures", map[string]any{"type": "ssh", "host-key": []any{"ssh-rsa AAAAB3NzaC1yc2EFixture"}}, []any{"rsa-sha2-512", "rsa-sha2-256", "ssh-rsa"}},
		{"several types without duplicates", map[string]any{"type": "ssh", "host-key": []any{ed25519, "ecdsa-sha2-nistp256 AAAAE2VjZHNhFixture", ed25519}}, []any{"ssh-ed25519", "ecdsa-sha2-nistp256"}},
		{"explicit list is kept", map[string]any{"type": "ssh", "host-key": []any{ed25519}, "host-key-algorithms": []any{"ecdsa-sha2-nistp256"}}, []any{"ecdsa-sha2-nistp256"}},
		{"unknown key type leaves the default", map[string]any{"type": "ssh", "host-key": []any{ed25519, "ssh-future AAAAFixture"}}, nil},
		{"no pinned key leaves the default", map[string]any{"type": "ssh"}, nil},
		{"other protocols are untouched", map[string]any{"type": "socks5", "host-key": []any{ed25519}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pinSSHHostKeyAlgorithms(tc.node)
			if got := tc.node["host-key-algorithms"]; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("host-key-algorithms = %#v, want %#v", got, tc.want)
			}
			if tc.node["type"] == "ssh" && tc.node["host-key"] == nil && tc.want != nil {
				t.Fatal("pinned keys were removed")
			}
		})
	}
}

// The runtime configuration also fixes SSH nodes imported by earlier versions.
func TestRuntimeBindingPinsSSHHostKeyAlgorithms(t *testing.T) {
	data := []byte("proxies:\n - {name: jump, type: ssh, server: 203.0.113.9, port: 22, username: u, host-key: ['ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFixtureKey']}\n")
	bound, err := BindInterface(data, "en7")
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := yaml.Unmarshal(bound, &config); err != nil {
		t.Fatal(err)
	}
	node := config["proxies"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(node["host-key-algorithms"], []any{"ssh-ed25519"}) || len(node["host-key"].([]any)) != 1 {
		t.Fatalf("runtime SSH node: %#v", node)
	}
}
