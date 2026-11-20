package workbench

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sylvinus/agent-vm/internal/uilocale"
)

func TestInterfaceLanguageStaysOnDesktopProfile(t *testing.T) {
	s := testServer(t)
	s.remote = newRemoteGateway(s.profile)
	s.remote.setRemote(true)
	t.Cleanup(s.remote.close)
	desktop := t.TempDir()
	s.appUpdateConfigPath = filepath.Join(desktop, "config.json")
	endpoint := "/api/client/language"
	path := filepath.Join(desktop, uilocale.PreferenceFile)
	if got := request(s, "GET", endpoint, "", "", false).Code; got != 401 {
		t.Fatalf("unauthenticated: %d", got)
	}
	if got := request(s, "POST", endpoint, "https://untrusted.example", `{"language":"ar"}`, true).Code; got != 403 {
		t.Fatalf("foreign origin: %d", got)
	}
	if got := request(s, "POST", endpoint, s.origin, `{"language":"en"}`, true).Code; got != 400 {
		t.Fatalf("unsupported language: %d", got)
	}
	get := func(language string) {
		t.Helper()
		w := request(s, "GET", endpoint, "", "", true)
		var response struct {
			Language string `json:"language"`
			Active   string `json:"active"`
			System   string `json:"system"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Language != language {
			t.Fatalf("GET language: %d %s", w.Code, w.Body.String())
		}
		if want := (uilocale.Preference{Language: language}).Active(response.System); response.Active != want || !uilocale.Valid(response.Active) {
			t.Fatalf("GET active language: %+v", response)
		}
	}
	get("system")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reading a fresh profile wrote a preference: %v", err)
	}
	for _, language := range []string{"ar", "system"} {
		w := request(s, "POST", endpoint, s.origin, `{"language":"`+language+`"}`, true)
		if w.Code != 200 {
			t.Fatalf("POST language: %d %s", w.Code, w.Body.String())
		}
		get(language)
		if pref := uilocale.Load(desktop); pref.Language != language {
			t.Fatalf("wrong desktop preference: %+v", pref)
		}
	}
	if _, err := os.Stat(filepath.Join(s.profile, uilocale.PreferenceFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrote into the workload profile: %v", err)
	}
}
