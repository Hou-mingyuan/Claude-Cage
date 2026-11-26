package main

import (
	"slices"
	"testing"

	"github.com/sylvinus/agent-vm/internal/cage"
)

func TestRelayEnvKeepsHostProxyVariablesAwayFromTheProxyCore(t *testing.T) {
	host := []string{"PATH=/usr/bin", "HTTP_PROXY=http://127.0.0.1:1", "https_proxy=http://127.0.0.1:2", "ALL_PROXY=socks5://127.0.0.1:3", "no_proxy=localhost"}
	config := cage.ConfigEnv + "=/profile/config.json"
	if got, want := relayEnv("proxy", "/profile/config.json", slices.Clone(host)), []string{"PATH=/usr/bin", config}; !slices.Equal(got, want) {
		t.Fatalf("proxy relay env = %q, want %q", got, want)
	}
	if got, want := relayEnv("controller", "/profile/config.json", slices.Clone(host)), append(slices.Clone(host), config); !slices.Equal(got, want) {
		t.Fatalf("controller relay env = %q, want %q", got, want)
	}
}
