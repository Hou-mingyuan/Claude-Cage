package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/guestos"
)

func TestWindowsGuestCannotFallBackToLinux(t *testing.T) {
	s := testServer(t)
	s.windowsGuest = &guestos.WindowsProfile{}
	if _, err := s.guestControl(context.Background(), "exit 0", ""); err == nil {
		t.Fatal("Windows ran Linux control")
	}
	if cmd := s.sshCommand(context.Background(), "-T"); cmd.Err == nil {
		t.Fatal("Windows created Linux SSH command")
	}
	r := httptest.NewRequest(http.MethodPost, s.origin+"/api/start", strings.NewReader(`{"guest_system":"debian-13"}`))
	r.Header.Set("Origin", s.origin)
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
	r.AddCookie(&http.Cookie{Name: s.cookieName() + "-desktop", Value: trustedViewProof(s.token)})
	response := httptest.NewRecorder()
	s.handler().ServeHTTP(response, r)
	if response.Code != http.StatusConflict || s.jobs.snapshot().ID != "" {
		t.Fatalf("wrong system start: %d", response.Code)
	}
	for _, path := range []string{"/api/mobile/state"} {
		response := request(s, http.MethodGet, path, s.origin, "", true)
		if response.Code != http.StatusNotImplemented {
			t.Fatalf("%s admitted: %d", path, response.Code)
		}
	}
}
func TestWindowsGuestCapabilitiesReportMissingFeatures(t *testing.T) {
	s := testServer(t)
	s.windowsGuest = &guestos.WindowsProfile{}
	s.ready = true
	s.windowsState.Available = true
	s.windowsState.ComponentsChecked = time.Now()
	s.windowsState.Capabilities = []string{"clipboard", "terminal"}
	s.windowsState.Components = map[string]windowsComponentState{"clipboard": {Installed: true, Ready: true}, "terminal": {Installed: true, Ready: true}}
	w := windowsBrowserRequestForTest(s, http.MethodGet, "/api/status", nil)
	var status struct {
		System       string `json:"guest_system"`
		Capabilities struct {
			Available   map[string]bool `json:"available"`
			Unavailable []string        `json:"unavailable"`
			Network     string          `json:"network"`
		} `json:"guest_capabilities"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.System != guestos.Windows11 || !status.Capabilities.Available["terminal"] || status.Capabilities.Network != "offline" {
		t.Fatalf("wrong status: %#v", status)
	}
	missing := map[string]bool{}
	for _, name := range status.Capabilities.Unavailable {
		missing[name] = true
	}
	for _, name := range []string{"projects", "browser", "network", "claude-cli", "claude-desktop"} {
		if !missing[name] {
			t.Fatalf("%s was reported supported", name)
		}
	}
}

func TestWindowsBrowserRegistrationRequiresInstalledRuntime(t *testing.T) {
	s := testServer(t)
	s.ready = true
	s.windowsState.Available = true
	s.windowsState.ComponentsChecked = time.Now()
	s.windowsState.Capabilities = []string{"browser", "project-mount"}
	available := s.windowsCapabilitiesLocked()["available"].(map[string]bool)
	if available["browser"] || available["editor"] {
		t.Fatal("registration alone advertised an installed browser/editor")
	}
	s.windowsState.Browser = windowsBrowserState{Installed: true, NetworkConfigured: true, EditorInstalled: true}
	s.windowsState.Components = map[string]windowsComponentState{"browser": {Installed: true, Ready: true}, "editor": {Installed: true, Ready: true}, "project-mount": {Installed: true, Ready: true}}
	available = s.windowsCapabilitiesLocked()["available"].(map[string]bool)
	if !available["browser"] || !available["editor"] {
		t.Fatal("installed runtime should be startable before its first window opens")
	}
	if s.windowsCapabilitiesLocked()["browser_running"] != false {
		t.Fatal("an unopened browser was reported running")
	}
}

func TestWindowsGateUsesBoundInstanceDirectory(t *testing.T) {
	s := testServer(t)
	s.windowsGuest = &guestos.WindowsProfile{InstanceDirectory: t.TempDir()}
	if s.instanceDir() != s.windowsGuest.InstanceDirectory {
		t.Fatal("Windows gate lookup fell back to a Linux instance directory")
	}
}

func TestWindowsSharedStartupAndFavoritesRoutes(t *testing.T) {
	s := testServer(t)
	s.windowsGuest = &guestos.WindowsProfile{}
	for _, path := range []string{"/api/favorites"} {
		w := windowsBrowserRequestForTest(s, "GET", path, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("shared Windows desktop GET %s: %d", path, w.Code)
		}
		if w := request(s, "GET", path, s.origin, "", true); w.Code != http.StatusForbidden {
			t.Fatalf("untrusted viewer reached %s: %d", path, w.Code)
		}
	}
	w := windowsBrowserRequestForTest(s, "POST", "/api/favorites", []byte(`{"action":"save","title":"Cage","url":"https://example.org/"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("save favorite: %d", w.Code)
	}
	list := windowsBrowserRequestForTest(s, "GET", "/api/favorites", nil)
	if !strings.Contains(list.Body.String(), "https://example.org/") {
		t.Fatal("Windows workbench did not restore the saved favorite")
	}
}

func TestWindowsColdStartWaitsForAgentButNotWrongIdentityOrLockedUser(t *testing.T) {
	for _, result := range []error{nil, errWindowsLoginRequired, errors.New("wrong VM")} {
		attempts := 0
		err := waitWindowsBoot(context.Background(), time.Millisecond, func(context.Context) error {
			attempts++
			if attempts == 1 {
				return errWindowsAgentPending
			}
			return result
		})
		if err != result || attempts != 2 {
			t.Fatalf("result=%v attempts=%d err=%v", result, attempts, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitWindowsBoot(ctx, time.Hour, func(context.Context) error { return errWindowsAgentPending }); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
}

func TestRemoteWindowsViewRespectsAccountAndProjectBoundaries(t *testing.T) {
	s := testServer(t)
	s.windowsGuest = &guestos.WindowsProfile{Mode: "remote"}
	get := func(path, proof string) int {
		r := httptest.NewRequest(http.MethodGet, s.origin+path, nil)
		r.Header.Set("Origin", s.origin)
		r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
		r.AddCookie(&http.Cookie{Name: "cage_remote_view_" + r.URL.Port(), Value: proof})
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, r)
		return w.Code
	}
	proof := remoteViewProof(s.token)
	if code := get("/api/status", trustedViewProof(s.token)); code != http.StatusForbidden {
		t.Fatalf("incorrect remote proof: %d", code)
	}
	if code := get("/api/status", proof); code != http.StatusOK {
		t.Fatalf("authenticated Windows remote session: %d", code)
	}
	if code := get("/api/projects/directories", proof); code != http.StatusNotImplemented {
		t.Fatalf("remote access to host project directory browser: %d", code)
	}
	s.account.trustedSession = true
	if code := get("/api/status", proof); code != http.StatusLocked {
		t.Fatalf("SSH proof used the desktop-only unlock: %d", code)
	}
	s.windowsGuest.Mode = "hyperv"
	if code := get("/api/status", proof); code != http.StatusForbidden {
		t.Fatalf("remote proof accepted for Hyper-V: %d", code)
	}
}

func TestStoppingRemoteWindowsRevokesBrokerAndTerminalAccess(t *testing.T) {
	s, gate := grantedToolUpdateServer(t)
	s.setJobForTest(Job{})
	s.windowsGuest = &guestos.WindowsProfile{Mode: "remote", InstanceDirectory: s.instanceDir()}
	if err := s.stopEnvironment(); err != nil {
		t.Fatal(err)
	}
	if gate.Status().Mode != cage.GateClosed {
		t.Fatal("detaching the controller left the remote broker open")
	}
	if _, err := s.createRemoteWindowsTerminal(terminalRequest{Kind: "shell"}); !errors.Is(err, errTerminalNotReady) {
		t.Fatalf("stopped environment attempted to start a remote shell: %v", err)
	}
}
