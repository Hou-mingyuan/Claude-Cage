package workbench

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/cage"
)

func TestDNSSettingsPersistBesideConfigAndSurviveLegacyClients(t *testing.T) {
	s := testServer(t)
	if !strings.HasPrefix(s.configPath, os.TempDir()) {
		t.Fatalf("config %q outside the test temp dir", s.configPath)
	}
	cfg, err := cage.Load(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	view := settingsView(cfg, s.preferences)
	if view.DNSFallbackURLs == nil || view.DNSBootstrapURLs == nil || view.DNSPublicFallback {
		t.Fatalf("defaults must be empty lists with the public fallback off: %+v", view)
	}
	view.DNSFallbackURLs = []string{"https://dns.example/dns-query"}
	view.DNSBootstrapURLs = []string{"https://223.5.5.5/dns-query"}
	view.DNSPublicFallback = true
	next, prefs, err := proposedSettings(view, cfg, s.profile)
	if err != nil {
		t.Fatal(err)
	}
	s.ready = true
	if err := s.apply(savedProfile{Config: next, Preferences: prefs}, false); err != nil {
		t.Fatal(err)
	}
	loaded, err := cage.Load(s.configPath)
	if err != nil || loaded.DNS != next.DNS {
		t.Fatalf("options not saved: %+v %v", loaded.DNS, err)
	}
	if got := loaded.DNSEndpoints(); len(got) != 3 || got[2] != cage.PublicDNSFallback {
		t.Fatalf("endpoints: %v", got)
	}
	raw, _ := os.ReadFile(s.configPath)
	if strings.Contains(string(raw), "dns.example") {
		t.Fatal("options written into the strict configuration")
	}
	// A client that predates these fields leaves them as saved.
	body, _ := json.Marshal(settingsView(loaded, s.preferences))
	var legacy map[string]any
	_ = json.Unmarshal(body, &legacy)
	delete(legacy, "dns_fallback_urls")
	delete(legacy, "dns_public_fallback")
	delete(legacy, "dns_bootstrap_urls")
	body, _ = json.Marshal(legacy)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/settings", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	v, ok := s.proposed(w, r)
	if !ok || v.Config.DNS != next.DNS {
		t.Fatalf("legacy client erased DNS options: ok=%v %+v %s", ok, v.Config.DNS, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(s.profile, cage.DNSOptionsFile)); err != nil {
		t.Fatal(err)
	}
}
