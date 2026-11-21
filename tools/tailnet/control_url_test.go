package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeControlURL(t *testing.T) {
	valid := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{"https://controlplane.tailscale.com", ""},
		{"https://controlplane.tailscale.com:443/", ""},
		{"https://Headscale.Example.COM/", "https://headscale.example.com"},
		{"https://headscale.example.com:443", "https://headscale.example.com"},
		{"https://headscale.example.com:8443/", "https://headscale.example.com:8443"},
	}
	for _, tc := range valid {
		got, err := normalizeControlURL(tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("normalizeControlURL(%q) = (%q, %v), want (%q, nil)", tc.in, got, err, tc.want)
		}
	}
	invalid := []string{
		"http://headscale.example.com",
		"https://user:pass@headscale.example.com",
		"https://headscale.example.com/path",
		"https://headscale.example.com?a=1",
		"https://headscale.example.com#frag",
		"https://headscale.example.com:0",
		"https://headscale.example.com:70000",
		"javascript:alert(1)",
		"headscale.example.com",
	}
	for _, raw := range invalid {
		if got, err := normalizeControlURL(raw); err == nil {
			t.Fatalf("normalizeControlURL(%q) unexpectedly succeeded: %q", raw, got)
		}
	}
}

func TestControlURLPersistenceIsolationAndFailClosed(t *testing.T) {
	dir := t.TempDir()
	n, err := newNode(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Official identity stays directly in dir (no migration of existing profiles).
	officialDir, err := n.stateDir()
	if err != nil || officialDir != dir {
		t.Fatalf("official stateDir = (%q, %v), want %q", officialDir, err, dir)
	}

	serverA := "https://hs-a.example.com:8443/"
	st, err := n.run(context.Background(), request{Action: "set-control-url", ControlURL: &serverA})
	if err != nil || st.ControlURL != "https://hs-a.example.com:8443" || st.Enabled {
		t.Fatalf("set server A: %+v %v", st, err)
	}
	dirA, err := n.stateDir()
	if err != nil || !strings.HasPrefix(dirA, filepath.Join(dir, "servers")+string(os.PathSeparator)) {
		t.Fatalf("server A dir not isolated: %q %v", dirA, err)
	}

	serverB := "https://hs-b.example.com"
	st, err = n.run(context.Background(), request{Action: "set-control-url", ControlURL: &serverB})
	if err != nil || st.ControlURL != serverB {
		t.Fatalf("set server B: %+v %v", st, err)
	}
	dirB, err := n.stateDir()
	if err != nil || dirB == dirA || dirB == dir {
		t.Fatalf("server B dir collided: dirA=%q dirB=%q", dirA, dirB)
	}

	// Reload from disk preserves control_url.
	reloaded, err := newNode(dir)
	if err != nil || reloaded.config.ControlURL != serverB {
		t.Fatalf("reload failed: %+v %v", reloaded, err)
	}

	// Clearing returns to official directory without deleting custom server dirs.
	empty := ""
	st, err = reloaded.run(context.Background(), request{Action: "set-control-url", ControlURL: &empty})
	if err != nil || st.ControlURL != "" {
		t.Fatalf("clear control_url: %+v %v", st, err)
	}
	backDir, err := reloaded.stateDir()
	if err != nil || backDir != dir {
		t.Fatalf("back to official dir: %q %v", backDir, err)
	}
	if _, err := os.Stat(dirA); err != nil {
		t.Fatalf("server A state directory was lost: %v", err)
	}

	// Corrupt/non-HTTPS settings.json fails closed rather than falling back to official.
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"enabled":true,"control_url":"http://evil.example"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newNode(dir); err == nil {
		t.Fatal("non-HTTPS control_url in settings.json must fail closed")
	}
}
