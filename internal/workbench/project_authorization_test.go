package workbench

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/remotehost"
)

func TestProjectAuthorizationProtectsCageStateAndAllowsCredentialDirectories(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	sshDir := filepath.Join(home, ".ssh")
	awsDir := filepath.Join(home, ".aws")
	configDir := filepath.Join(home, ".config", "demo")
	appDataDir := filepath.Join(home, "AppData", "Local", "demo")
	siblingState := filepath.Join(home, ".claude-cage", "state")
	for _, directory := range []string{sshDir, awsDir, configDir, appDataDir, siblingState} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	s := testServer(t)
	s.ready = true

	// Cage's own configuration/state directory remains protected.
	body, _ := json.Marshal(map[string]any{"workspace": siblingState, "add": true})
	response := desktopRequestForTest(s, http.MethodPost, "/api/projects/open", body)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "共享") {
		t.Fatalf("private state admission: status=%d body=%s", response.Code, response.Body.String())
	}
	paths, err := s.projectPaths()
	if err != nil || slices.Contains(paths, siblingState) || len(s.projectShares) != 0 {
		t.Fatalf("rejected project retained a grant or connection: %v", err)
	}
	response = desktopRequestForTest(s, http.MethodGet, "/api/projects/directories?path="+url.QueryEscape(siblingState), nil)
	if response.Code == http.StatusOK {
		var listing struct{ Selectable bool }
		if json.Unmarshal(response.Body.Bytes(), &listing) != nil || listing.Selectable {
			t.Fatal("directory browser offered a protected Cage state directory")
		}
	}

	// Folders containing credentials or application data are allowed to be shared.
	for _, directory := range []string{sshDir, awsDir, configDir, appDataDir} {
		if _, err := validateWorkspace(directory, s.profile); err != nil {
			t.Fatalf("credential directory %s rejected: %v", directory, err)
		}
		response = desktopRequestForTest(s, http.MethodGet, "/api/projects/directories?path="+url.QueryEscape(directory), nil)
		if response.Code != http.StatusOK {
			t.Fatalf("directory browser rejected %s: status=%d body=%s", directory, response.Code, response.Body.String())
		}
		var listing struct{ Selectable bool }
		if json.Unmarshal(response.Body.Bytes(), &listing) != nil || !listing.Selectable {
			t.Fatalf("directory browser marked %s as unselectable", directory)
		}
	}

	// Cloud project sharing also allows folders containing configured SSH keys.
	s.remote = newRemoteGateway(s.profile)
	t.Cleanup(func() { s.remote.manager.Close() })
	keyPath := filepath.Join(sshDir, "id_ed25519")
	if err := os.WriteFile(keyPath, []byte("test-key"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.remote.store.Upsert(remotehost.HostProfile{
		Name: "cloud",
		Host: "127.0.0.1",
		Port: 22,
		User: "cage",
		Auth: remotehost.AuthConfig{Kind: "key_file", IdentityFile: keyPath},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.clientProjectRoot(sshDir); err != nil {
		t.Fatalf("cloud share rejected folder containing SSH key: %v", err)
	}
}

func TestProjectRestoreRevalidatesExistingPrivateStateGrant(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	s := testServer(t)
	siblingState := filepath.Join(home, ".claude-cage", "state")
	if err := os.MkdirAll(siblingState, 0700); err != nil {
		t.Fatal(err)
	}
	// Existing versions could persist a private state grant. A new connection
	// must apply the current authorization rule before starting a transport.
	if err := s.rememberSharedProject(siblingState); err != nil {
		t.Fatal(err)
	}
	s.restoreProjectShares()
	if len(s.projectShares) != 0 || len(s.projectErrors) != 1 || s.projectErrors[0].Path != siblingState {
		t.Fatalf("old private state grant was not rejected on restore: %+v", s.projectErrors)
	}
	if !strings.Contains(s.projectErrors[0].Message, "共享") {
		t.Fatalf("restore did not report the authorization failure: %+v", s.projectErrors)
	}
}
