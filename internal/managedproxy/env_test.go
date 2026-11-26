package managedproxy

import (
	"slices"
	"testing"
)

func TestWithoutHostProxyEnvDropsProxyVariablesInAnyCase(t *testing.T) {
	env := []string{
		"PATH=/usr/bin", "http_proxy=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:2", "All_Proxy=socks5://127.0.0.1:3",
		"NO_PROXY=localhost", "ftp_proxy=http://127.0.0.1:4", "=C:=C:\\", "HTTP_PROXY_EXTRA=kept", "CLAUDE_CAGE_CONFIG=/profile/config.json",
	}
	got := WithoutHostProxyEnv(env)
	want := []string{"PATH=/usr/bin", "=C:=C:\\", "HTTP_PROXY_EXTRA=kept", "CLAUDE_CAGE_CONFIG=/profile/config.json"}
	if !slices.Equal(got, want) {
		t.Fatalf("WithoutHostProxyEnv() = %q, want %q", got, want)
	}
	if env[1] != "http_proxy=http://127.0.0.1:1" {
		t.Fatal("input environment was modified")
	}
}
