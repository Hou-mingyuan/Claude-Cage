package remotehost

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/host"
)

func TestAgentSocketValidationRules(t *testing.T) {
	for _, tc := range []struct {
		goos, value string
		ok          bool
	}{
		{"darwin", "/Users/me/.bitwarden-ssh-agent.sock", true},
		{"linux", "/run/user/1000/gnupg/S.gpg-agent.ssh", true},
		{"linux", "relative/agent.sock", false},
		{"linux", "~/.ssh/agent.sock", false},
		{"linux", "/tmp/agent\n.sock", false},
		{"linux", "/" + strings.Repeat("a", 106), true},
		{"linux", "/" + strings.Repeat("a", 107), false},
		{"darwin", "/" + strings.Repeat("a", 103), false},
		{"windows", `\\.\pipe\openssh-ssh-agent`, true},
		{"windows", `\\.\PIPE\bitwarden-ssh-agent`, true},
		{"windows", `\\.\pipe\`, false},
		{"windows", `\\server\pipe\agent`, false},
		{"windows", `C:\Users\me\agent.sock`, false},
		{"windows", `\\.\pipe\a/b`, false},
		{"windows", `\\.\pipe\` + strings.Repeat("a", 250), false},
	} {
		if got := validAgentSocket(tc.goos, tc.value); got != tc.ok {
			t.Errorf("validAgentSocket(%q, %q) = %v, want %v", tc.goos, tc.value, got, tc.ok)
		}
	}
}

func TestAgentSocketIsOptionalAndOnlyKeptForAgentProfiles(t *testing.T) {
	dir := t.TempDir()
	socket := "/home/me/agent.sock"
	if runtime.GOOS == "windows" {
		socket = `\\.\pipe\bitwarden-ssh-agent`
	}
	// A profile written before the field existed is read unchanged.
	old := `{"version":1,"hosts":[{"id":"` + newID() + `","name":"old","host":"localhost","port":22,"user":"cage","auth":{"kind":"agent"},"executable":"cage"}]}`
	if err := host.CreatePrivateFile(filepath.Join(dir, "remote-hosts.json"), []byte(old)); err != nil {
		t.Fatal(err)
	}
	store := NewStore(dir)
	hosts, err := store.List()
	if err != nil || len(hosts) != 1 || hosts[0].Auth.AgentSocket != "" {
		t.Fatalf("old profile = %+v, %v", hosts, err)
	}
	saved, err := store.Upsert(HostProfile{Name: "agent", Host: "localhost", User: "cage", Auth: AuthConfig{Kind: "agent", AgentSocket: " " + socket + " "}})
	if err != nil || saved.Auth.AgentSocket != socket {
		t.Fatalf("saved = %+v, %v", saved.Auth, err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "remote-hosts.json"))
	if !strings.Contains(string(data), `"agent_socket":`) || strings.Count(string(data), `"agent_socket"`) != 1 {
		t.Fatalf("stored file = %s", data)
	}
	keyFile := filepath.Join(dir, "id")
	switched, err := store.Upsert(HostProfile{ID: saved.ID, Name: "agent", Host: "localhost", User: "cage", Auth: AuthConfig{Kind: "key_file", IdentityFile: keyFile, AgentSocket: socket}})
	if err != nil || switched.Auth.AgentSocket != "" {
		t.Fatalf("key file profile kept agent socket: %+v, %v", switched.Auth, err)
	}
	_, err = store.Upsert(HostProfile{Name: "bad", Host: "localhost", User: "cage", Auth: AuthConfig{Kind: "agent", AgentSocket: "agent.sock"}})
	var invalid *Error
	if !errors.As(err, &invalid) || invalid.Code != "invalid_profile" {
		t.Fatalf("relative socket accepted: %v", err)
	}
	var stored storedHosts
	data, _ = os.ReadFile(filepath.Join(dir, "remote-hosts.json"))
	if json.Unmarshal(data, &stored) != nil || len(stored.Hosts) != 2 {
		t.Fatalf("rejected profile changed the file: %s", data)
	}
}
