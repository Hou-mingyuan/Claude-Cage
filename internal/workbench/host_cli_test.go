package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestHostCLIRequiresLocalProof(t *testing.T) {
	s := testServer(t)
	rec := request(s, "GET", "/api/host-cli", "", "", true)
	if rec.Code != http.StatusForbidden {
		t.Fatal(rec.Code)
	}
}

func TestHostCLIConsoleFiltersAcrossFrames(t *testing.T) {
	var p cliScreen
	var got []byte
	for _, chunk := range [][]byte{[]byte("开\x1b]52;"), []byte("c;QQ==\x1b"), []byte("\\始\x1b[38;5;12m"), []byte("X\x1b[6n"), []byte("\x1bPblocked\x1b\\")} {
		got = append(got, p.filter(chunk, false)...)
	}
	if string(got) != "开始\x1b[38;5;12mX\x1b[6n" {
		t.Fatalf("%q", got)
	}
	if got := p.filter([]byte("\x1b[6n\x1b[?1049h"), true); string(got) != "\x1b[?1049h" {
		t.Fatalf("history: %q", got)
	}
}

func TestHostCLILoginUsesGuestBrowserAndSelectsVerifiedAccount(t *testing.T) {
	s := agentTestController(t)
	s.ready = true
	s.claudeAccounts.probe = func(context.Context, string) (claudeGuestSnapshot, error) {
		return claudeGuestSnapshot{LoggedIn: true, Email: "fixture@example.test", Organization: "test"}, nil
	}
	type openTab struct{ URL, Account string }
	requested := make(chan openTab, 1)
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var opened openTab
		if r.URL.Path != "/new" || r.Method != "POST" || json.NewDecoder(r.Body).Decode(&opened) != nil {
			http.Error(w, "invalid guest browser request", 400)
			return
		}
		requested <- opened
		_, _ = w.Write([]byte(`{"id":"fixture-browser-tab"}`))
	}))
	defer guest.Close()
	s.browserControl = localBrowserClient(t, s.lifetimeContext(), guest)
	defer s.browserControl.close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	client, err := agentConnect(ctx, s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	call := func(action, selector string) string {
		t.Helper()
		var out, stderr bytes.Buffer
		n, err := cliExchange(ctx, client, hostCLIRequest{Action: action, Account: selector}, strings.NewReader(""), &out, &stderr, nil)
		if err != nil || n != 0 {
			t.Fatalf("%s: %v (%d) %s", action, err, n, stderr.String())
		}
		return out.String()
	}
	call("login", "")
	s.mu.Lock()
	focus := s.hostCLIBrowser
	s.mu.Unlock()
	pin, err := s.cliPinnedAccount()
	opened := <-requested
	if err != nil || !validClaudeID(pin) || opened.URL != "https://claude.ai" || opened.Account != pin || focus.Tab != "fixture-browser-tab" {
		t.Fatalf("pin=%q err=%v tab=%+v guest=%+v", pin, err, focus, opened)
	}
	list, err := s.cliAccountRoster()
	if err != nil || len(list.Accounts) != 1 || list.Accounts[0].Enabled {
		t.Fatalf("login changed scheduling: %+v %v", list, err)
	}
	if got := call("accounts", ""); !strings.Contains(got, "#1") || !strings.Contains(got, "[CLI]") {
		t.Fatal(got)
	}
	call("use-account", "auto")
	if got := call("accounts", ""); strings.Contains(got, "[CLI]") {
		t.Fatal(got)
	}
	call("use-account", "#1")
	if got, err := s.cliPinnedAccount(); err != nil || got != pin {
		t.Fatalf("use: %q %v", got, err)
	}
	if projects, err := s.projectPaths(); err != nil || len(projects) > 0 {
		t.Fatalf("global action created projects: %v %v", projects, err)
	}
}

func TestHostCLIAdditionalDirectoriesAreExplicitAndReferenceCounted(t *testing.T) {
	s := testServer(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, extra := filepath.Join(root, "project"), filepath.Join(root, "library")
	for _, folder := range []string{dir, extra} {
		if err := os.Mkdir(folder, 0700); err != nil {
			t.Fatal(err)
		}
	}
	s.projectShares = map[string]*projectShare{extra: {}}
	first := &hostCLIUse{workspace: dir, folders: []string{dir}}
	args := []string{"-p", "--append-system-prompt", "--add-dir", extra, "--", "--add-dir", extra}
	if got, err := s.cliArguments(args, dir, first); err != nil || !slices.Equal(got, args) || len(first.extra) != 0 {
		t.Fatalf("opaque arguments granted a directory: %q %v %v", got, first.extra, err)
	}
	second := &hostCLIUse{workspace: dir, folders: []string{dir}}
	for _, use := range []*hostCLIUse{first, second} {
		args := []string{"--settings", filepath.Join(extra, "settings.json"), "--add-dir", "../library"}
		got, err := s.cliArguments(args, dir, use)
		if err != nil || got[3] != guestPath(extra) || !slices.Equal(use.extra, []string{extra}) {
			t.Fatalf("grant: %q %+v %v", got, use, err)
		}
		s.registerCLIUse(use)
	}
	if err := s.checkCLIHostProject(dir); err != nil {
		t.Fatal(err)
	}
	if err := s.checkCLIHostProject(root); err == nil {
		t.Fatal("allowed a host tool bound to a different workspace")
	}
	s.releaseCLIProcess(first)
	if s.projectShares[extra] == nil {
		t.Fatal("released a mount held by the second process")
	}
	s.releaseCLIProcess(second)
	if s.projectShares[extra] != nil {
		t.Fatal("temporary mount survived its last process")
	}
	if _, err := s.cliArguments([]string{"--add-dir", s.profile}, dir, first); err == nil {
		t.Fatal("private profile was shareable")
	}
}
