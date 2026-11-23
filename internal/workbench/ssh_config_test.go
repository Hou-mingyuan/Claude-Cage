package workbench

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sylvinus/agent-vm/internal/remotehost"
)

func TestSSHConfigImportKeepsLocalSessionAndOriginBoundary(t *testing.T) {
	s := testServer(t)
	s.remote = newRemoteGateway(s.profile)
	t.Cleanup(s.remote.close)
	for _, method := range []string{"GET", "POST"} {
		if w := request(s, method, "/api/client/ssh-config", s.origin, "{}", false); w.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s: %d", method, w.Code)
		}
	}
	if w := request(s, "POST", "/api/client/ssh-config", "https://untrusted.example", "{}", true); w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin import: %d", w.Code)
	}
}

func TestSSHConfigPreviewAndImport(t *testing.T) {
	home := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
		t.Setenv("USERNAME", "local-user")
	} else {
		t.Setenv("HOME", home)
	}
	if err := os.Mkdir(filepath.Join(home, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, ".ssh", "config")
	text := "Host dev prod\n HostName \" %h.example \"\n User \" remote \"\n IdentityFile /never-read/SECRET_KEY\nHost jump\n ProxyCommand command SECRET_COMMAND\n"
	if err := os.WriteFile(config, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	s := testServer(t)
	s.remote = newDesktopGateway(s.profile, nil)
	t.Cleanup(s.remote.close)
	s.account.lock()
	var forwarded atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forwarded.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(upstream.Close)
	ctx, cancel := context.WithCancel(t.Context())
	desktop := s.remote.desktop
	desktop.active = &desktopTarget{
		id: desktop.generation, mode: "remote", ctx: ctx, cancel: cancel,
		session: &remotehost.Session{
			Origin: upstream.URL, Client: upstream.Client(), Transport: upstream.Client().Transport,
			Context: ctx, Manifest: remotehost.PublicManifest{Platform: "linux", InstanceID: strings.Repeat("a", 64)},
		},
	}
	before := s.remote.context()
	if before.Mode != "remote" || before.State.State != "connected" {
		t.Fatalf("fixture did not select a remote workbench: %+v", before)
	}
	// The local import API still works with a remote workbench selected.
	w := request(s, "GET", "/api/client/ssh-config", "", "", true)
	var preview remotehost.SSHConfigPreview
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &preview) != nil || len(preview.Hosts) != 2 || len(preview.Skipped) != 1 {
		t.Fatalf("preview: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "SECRET") {
		t.Fatal("preview leaked a referenced key path or command")
	}
	if h := preview.Hosts[0]; h.Name != "dev" || h.Host != "dev.example" || h.User != "remote" {
		t.Fatalf("preview was not normalized: %+v", h)
	}
	if _, err := os.Stat(filepath.Join(s.profile, "remote-hosts.json")); !os.IsNotExist(err) {
		t.Fatal("preview persisted profiles")
	}
	payload := func(revision string, aliases []string) string {
		data, _ := json.Marshal(map[string]any{"revision": revision, "aliases": aliases})
		return string(data)
	}
	for _, body := range []string{payload(preview.Revision, []string{"jump"}), payload(preview.Revision, []string{"dev", "dev"}), payload("stale", []string{"dev"}), `{"path":"/arbitrary","aliases":["dev"]}`} {
		if got := request(s, "POST", "/api/client/ssh-config", s.origin, body, true); got.Code == 200 {
			t.Fatalf("invalid selection accepted: %s", body)
		}
	}
	if _, err := os.Stat(filepath.Join(s.profile, "remote-hosts.json")); !os.IsNotExist(err) {
		t.Fatal("rejected request persisted profiles")
	}
	if err := os.WriteFile(config, []byte(text+"# changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := request(s, "POST", "/api/client/ssh-config", s.origin, payload(preview.Revision, []string{"dev"}), true); got.Code != 409 {
		t.Fatalf("changed config accepted: %d %s", got.Code, got.Body.String())
	}
	if err := os.WriteFile(config, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	body := payload(preview.Revision, []string{"dev"})
	w = request(s, "POST", "/api/client/ssh-config", s.origin, body, true)
	var result struct {
		Imported []remotehost.HostProfile `json:"imported"`
		Skipped  int                      `json:"skipped"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Imported) != 1 || result.Imported[0].Auth.Kind != "agent" || result.Imported[0].Auth.IdentityFile != "" {
		t.Fatalf("import: %d %s", w.Code, w.Body.String())
	}
	w = request(s, "POST", "/api/client/ssh-config", s.origin, body, true)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Imported) != 0 || result.Skipped != 1 {
		t.Fatalf("repeat: %d %s", w.Code, w.Body.String())
	}
	stored, err := os.ReadFile(filepath.Join(s.profile, "remote-hosts.json"))
	if err != nil || strings.Contains(string(stored), "SECRET") || strings.Contains(string(stored), "identity_file") {
		t.Fatalf("stored private material: %s %v", stored, err)
	}
	if state := s.remote.context(); state.Mode != before.Mode || state.ConnectionID != before.ConnectionID || state.State.State != before.State.State || state.Candidate != nil {
		t.Fatalf("import changed the active connection: %+v", state)
	}
	if forwarded.Load() != 0 {
		t.Fatal("import requests reached the remote workbench")
	}
}
