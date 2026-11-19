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

func TestClockPreferencePersistsWithoutChangingRuntime(t *testing.T) {
	s := testServer(t)
	cfg, err := cage.Load(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if settingsView(cfg, s.preferences).Clock12Hour {
		t.Fatal("clock must default to 24-hour time")
	}
	s.ready = true
	p := s.preferences
	p.Clock12Hour = true
	if err := s.apply(savedProfile{cfg, p}, true); err != nil {
		t.Fatal(err)
	}
	after, err := cage.Load(s.configPath)
	if err != nil || after != cfg || !s.ready {
		t.Fatalf("clock-only save changed runtime: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(s.profile, "workbench.json"))
	var saved Preferences
	if err != nil || json.Unmarshal(b, &saved) != nil || !saved.Clock12Hour || !settingsView(cfg, s.preferences).Clock12Hour {
		t.Fatal("12-hour preference not persisted")
	}
	// Clients predating this setting must preserve the saved choice.
	body, _ := json.Marshal(settingsView(cfg, p))
	var legacy map[string]any
	_ = json.Unmarshal(body, &legacy)
	delete(legacy, "clock_12_hour")
	body, _ = json.Marshal(legacy)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/settings", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	proposal, ok := s.proposed(w, r)
	if !ok || !proposal.Preferences.Clock12Hour {
		t.Fatalf("older client reset clock preference: %s", w.Body.String())
	}
}
