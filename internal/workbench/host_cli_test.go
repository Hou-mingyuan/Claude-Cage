package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHostCLIAccountNamesAndRenaming(t *testing.T) {
	s := agentTestController(t)
	a := claudeAccount{ID: strings.Repeat("a", 24), Number: 3, Label: "old note", Name: "Sarah", Source: "managed", Email: "member@example.test", Status: "ready"}
	if err := s.saveClaudeAccounts(claudeAccountList{Accounts: []claudeAccount{a}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	call := func(action string, args ...string) (int, string, error) {
		t.Helper()
		out, err := os.CreateTemp(t.TempDir(), "stdout")
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		code, err := HostCLI(ctx, s.configPath, action, args, null, out, null)
		data, readErr := os.ReadFile(out.Name())
		if readErr != nil {
			t.Fatal(readErr)
		}
		return code, string(data), err
	}
	if code, _, err := call("account", "use", "#03"); code != 0 || err != nil {
		t.Fatalf("select display number: %d %v", code, err)
	}
	if code, _, err := call("account", "rename", "03", "工作"); code != 0 || err != nil {
		t.Fatalf("rename: %d %v", code, err)
	}
	code, output, err := call("account", "list")
	fields := strings.Fields(output)
	for _, want := range []string{"ID", "NAME", "EMAIL", "#3", a.ID, "工作", a.Email, "[CLI]"} {
		if err != nil || code != 0 || !slices.Contains(fields, want) {
			t.Fatalf("missing %q in account list: %q %d %v", want, output, code, err)
		}
	}
	for _, selector := range []string{"Sarah", "old", "missing"} {
		if code, _, err := call("run-by-name", selector, "-p", "hello"); code == 0 || err == nil || !strings.Contains(err.Error(), "account list") {
			t.Fatalf("unknown name %q: %d %v", selector, code, err)
		}
	}
	if s.jobs.snapshot().ID != "" || s.vmRunning() {
		t.Fatal("a missing name or an account edit started a guest operation")
	}
	if projects, err := s.projectPaths(); err != nil || len(projects) != 0 {
		t.Fatalf("account management shared a directory: %q %v", projects, err)
	}
	list, err := s.cliAccountRoster()
	if err != nil {
		t.Fatal(err)
	}
	for _, selector := range []string{a.ID, "3", "#03", "工作", a.Email, a.Label} {
		if id, err := resolveCLIAccount(list, selector); err != nil || id != a.ID {
			t.Fatalf("explicit selector %q: %q %v", selector, id, err)
		}
	}
	if _, err := list.byCLIName(a.Email); err == nil {
		t.Fatal("a verified email was implicitly used as a shortcut name")
	}
	s.account.lock()
	if code, _, err := call("account", "rename", "3", "blocked"); code == 0 || err == nil {
		t.Fatal("locked workbench allowed a rename")
	}
}

func TestHostCLINamedColdStartRejectsReassignedName(t *testing.T) {
	s := agentTestController(t)
	first := claudeAccount{Number: 1, ID: strings.Repeat("a", 24), Name: "work", Email: "one@example.test", Source: "managed", Status: "ready"}
	second := claudeAccount{Number: 2, ID: strings.Repeat("b", 24), Name: "other", Email: "two@example.test", Source: "managed", Status: "ready"}
	if err := s.saveClaudeAccounts(claudeAccountList{Accounts: []claudeAccount{first, second}}); err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(filepath.Join(s.profile, "initialization.json"), initializationRecord{Version: 1, Completed: true}); err != nil {
		t.Fatal(err)
	}
	s.deviceReset.startEnv = func(bool) error {
		markTestVMRunning(t, s)
		s.mu.Lock()
		s.ready = true
		s.mu.Unlock()
		s.claudeAccounts.mu.Lock()
		defer s.claudeAccounts.mu.Unlock()
		first.Name, second.Name = "renamed", "work"
		return s.saveClaudeAccounts(claudeAccountList{Accounts: []claudeAccount{first, second}})
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	code, err := HostCLI(ctx, s.configPath, "run-by-name", []string{"work", "-p", "private prompt"}, null, null, null)
	if code == 0 || err == nil || !strings.Contains(err.Error(), "账号已变化") {
		t.Fatalf("reassigned name: code=%d err=%v", code, err)
	}
	if projects, err := s.projectPaths(); err != nil || len(projects) != 0 {
		t.Fatalf("reassigned name created a project: %q %v", projects, err)
	}
}

func TestHostCLIStartBootsOnceWithoutSharingOrLoggingIn(t *testing.T) {
	s := agentTestController(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := writePrivate(filepath.Join(s.profile, "initialization.json"), initializationRecord{Version: 1, Completed: true}); err != nil {
		t.Fatal(err)
	}
	boots := 0
	s.deviceReset.startEnv = func(bool) error {
		markTestVMRunning(t, s)
		s.mu.Lock()
		s.ready = true
		s.mu.Unlock()
		boots++
		return nil
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()

	s.account.lock()
	if code, err := HostCLI(ctx, s.configPath, "start", nil, null, null, null); code == 0 || err == nil || !strings.Contains(err.Error(), "解锁") || boots != 0 {
		t.Fatalf("locked start: code=%d error=%v boots=%d", code, err, boots)
	}
	s.account.mu.Lock()
	s.account.setKey(make([]byte, 32))
	s.account.mu.Unlock()
	for range 2 {
		if code, err := HostCLI(ctx, s.configPath, "start", nil, null, null, null); code != 0 || err != nil {
			t.Fatalf("start: code=%d err=%v", code, err)
		}
	}
	if boots != 1 || !s.vmRunning() || s.hostCLIBrowser.ID != "" {
		t.Fatalf("boots=%d vm=%v login request=%q", boots, s.vmRunning(), s.hostCLIBrowser.ID)
	}
	if projects, err := s.projectPaths(); err != nil || len(projects) > 0 {
		t.Fatalf("start shared a project: %v %v", projects, err)
	}
}

func TestRebuildDiskRequiresPrivateLocalCLI(t *testing.T) {
	s := testServer(t)
	rec := request(s, "POST", "/api/start", s.origin, `{"reset_disk_token":"not-a-launcher-token"}`, true)
	if rec.Code != http.StatusForbidden || s.jobs.snapshot().ID != "" {
		t.Fatalf("ordinary session submitted disk rebuild: code=%d, job=%+v", rec.Code, s.jobs.snapshot())
	}
}

func TestHostCLIStartRequiresRunningGuestAndReportsOffline(t *testing.T) {
	s := agentTestController(t)
	if err := writePrivate(filepath.Join(s.profile, "initialization.json"), initializationRecord{Version: 1, Completed: true}); err != nil {
		t.Fatal(err)
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var guestStarts atomic.Bool
	s.deviceReset.startEnv = func(bool) error {
		if guestStarts.Load() {
			markTestVMRunning(t, s)
		}
		s.mu.Lock()
		s.ready = true
		s.mu.Unlock()
		return networkCheckError{errors.New("exit check unavailable")}
	}
	logFile := filepath.Join(t.TempDir(), "stderr")
	log, err := os.Create(logFile)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	if code, err := HostCLI(ctx, s.configPath, "start", nil, null, null, log); code == 0 || err == nil {
		t.Fatal("start reported ready while the VM was stopped")
	}
	guestStarts.Store(true)
	if code, err := HostCLI(ctx, s.configPath, "start", nil, null, null, log); code != 0 || err != nil {
		t.Fatalf("offline start: code=%d err=%v", code, err)
	}
	output, err := os.ReadFile(logFile)
	if err != nil || !strings.Contains(string(output), "外网尚未放行") {
		t.Fatalf("missing offline status: %q err=%v", output, err)
	}
}

func TestHostCLIStatusReportsRunningVMLocked(t *testing.T) {
	s := agentTestController(t)
	s.instanceID = randomToken()
	markTestVMRunning(t, s)
	s.account.lock()
	out, err := os.CreateTemp(t.TempDir(), "status")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if code, err := HostCLI(ctx, s.configPath, "status", nil, out, out, out); code != 0 || err != nil {
		t.Fatalf("status: code=%d err=%v", code, err)
	}
	got, err := os.ReadFile(out.Name())
	_, unlocked := s.account.access()
	if err != nil || unlocked || !strings.Contains(string(got), "环境正在运行") || !strings.Contains(string(got), "解锁") {
		t.Fatalf("status=%q unlocked=%v err=%v", got, unlocked, err)
	}
}

func TestHostCLIConcurrentLaunchersSpawnOneService(t *testing.T) {
	exe := os.Getenv("CAGE_TEST_CAGE_CLI")
	if !filepath.IsAbs(exe) {
		t.Skip("set CAGE_TEST_CAGE_CLI to a built Cage executable")
	}
	config := filepath.Join(t.TempDir(), "profile", "config.json")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t.Cleanup(func() {
		if _, err := PrepareUpdate(config, false); err != nil {
			t.Error(err)
		}
	})
	type launch struct {
		pid     int
		started bool
		err     error
	}
	const count = 4
	begin := make(chan struct{})
	results := make(chan launch, count)
	for range count {
		go func() {
			<-begin
			c, started, err := startControllerExecutable(ctx, config, exe)
			r := launch{started: started, err: err}
			if c != nil {
				r.pid = c.record.PID
				c.close()
			}
			results <- r
		}()
	}
	close(begin)
	var pid, launches int
	for range count {
		r := <-results
		if r.err != nil || r.pid == 0 || (pid != 0 && r.pid != pid) {
			t.Errorf("launch returned %+v, prior PID %d", r, pid)
		}
		pid = r.pid
		if r.started {
			launches++
		}
	}
	if launches != 1 {
		t.Fatalf("%d callers started a service, want one", launches)
	}
}

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
