//go:build !windows

package workbench

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/creack/pty"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

func TestHostCLIProtocolRunsNativeArgvAndPipes(t *testing.T) {
	s := agentTestController(t)
	s.ready = true
	proj, resolveErr := filepath.EvalSymlinks(t.TempDir())
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	dir := filepath.Join(proj, "子 目录")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	// Simulate the guest's mount transport with the existing SSH fixture.
	// The CLI launch, stdin/EOF, two output streams and exit status travel
	// through the real authenticated controller and the OpenSSH executable.
	probe := filepath.Join(home, "claude")
	script := "#!/usr/bin/env python3\nimport sys,os,json\nprint(json.dumps([sys.argv[1:],sys.stdin.read(),os.getcwd()]))\nprint('diagnostic',file=sys.stderr)\nsys.exit(27)\n"
	if err := os.WriteFile(probe, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	s.claudeAccounts.probe = func(context.Context, string) (claudeGuestSnapshot, error) {
		return claudeGuestSnapshot{LoggedIn: true, Email: "qa@example.test"}, nil
	}
	s.projectShares = map[string]*projectShare{proj: {done: make(chan struct{})}}
	if err := s.rememberSharedProject(proj); err != nil {
		t.Fatal(err)
	}
	mobileTestSSHRewrite(t, s, home, []string{"HOME=" + home, "PATH=/usr/bin:/bin"}, func(command string) string {
		return strings.Replace(command, "exec claude ", "exec "+shellescape.Quote(probe)+" ", 1)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, e := agentConnect(ctx, s.configPath)
	if e != nil {
		t.Fatal(e)
	}
	defer c.close()
	args := []string{"-p", "--output-format", "json", "two words", "中文", "", "a\nb", "--", "login"}
	var out, errorsOut bytes.Buffer
	pipe := strings.Repeat("流\n", 50_000)
	code, e := cliExchange(ctx, c, hostCLIRequest{Action: "run", Args: args, Directory: dir}, strings.NewReader(pipe), &out, &errorsOut, nil)
	if e != nil || code != 27 || errorsOut.String() != "diagnostic\n" {
		t.Fatalf("code=%d err=%v stdout=%q stderr=%q", code, e, out.String(), errorsOut.String())
	}
	var actual []json.RawMessage
	if e = json.Unmarshal(out.Bytes(), &actual); e != nil || len(actual) != 3 {
		t.Fatalf("invalid stdout %q", out.String())
	}
	var argv []string
	var input, cwd string
	_ = json.Unmarshal(actual[0], &argv)
	_ = json.Unmarshal(actual[1], &input)
	_ = json.Unmarshal(actual[2], &cwd)
	resolved, _ := filepath.EvalSymlinks(dir)
	if !reflect.DeepEqual(argv, args) || input != pipe || cwd != resolved {
		t.Errorf("argv=%q stdin size=%d cwd=%q want=%q", argv, len(input), cwd, resolved)
	}
}

func TestHostCLIMatchesNativeProfileDirectoryAlias(t *testing.T) {
	dir := t.TempDir()
	alias := filepath.Join(t.TempDir(), "profile-alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if !sameControllerConfig(path, filepath.Join(alias, "config.json")) || sameControllerConfig(path, filepath.Join(alias, "other.json")) || sameControllerConfig(path, filepath.Join(t.TempDir(), "config.json")) {
		t.Fatal("incorrect controller configuration identity")
	}
}

func TestHostCLIFirstLoginWaitsForVerifiedManagedAccount(t *testing.T) {
	for _, tc := range []struct {
		name       string
		authorized bool
	}{{"without Claude authorization", false}, {"with Claude authorization", true}} {
		t.Run(tc.name, func(t *testing.T) {
			s := agentTestController(t)
			s.ready = true
			s.claudeAccounts.probe = func(_ context.Context, id string) (claudeGuestSnapshot, error) {
				if id == "default" || !tc.authorized {
					return claudeGuestSnapshot{}, nil
				}
				return claudeGuestSnapshot{LoggedIn: true, Email: "first@example.test"}, nil
			}
			mobileTestSSHRewrite(t, s, t.TempDir(), []string{"PATH=/usr/bin:/bin"}, func(command string) string {
				if strings.Contains(command, "cage-claude-account") && strings.Contains(command, "login") {
					return "exit 0"
				}
				return "exit 1"
			})
			t.Cleanup(s.terminals.close)
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			client, err := agentConnect(ctx, s.configPath)
			if err != nil {
				t.Fatal(err)
			}
			defer client.close()
			var stderr bytes.Buffer
			code, err := cliExchange(ctx, client, hostCLIRequest{Action: "login"}, strings.NewReader(""), io.Discard, &stderr, nil)
			if !tc.authorized {
				if err == nil || code == 0 || !strings.Contains(err.Error(), "Claude") || strings.Contains(stderr.String(), "Claude 账号已登录") {
					t.Fatalf("unexpected result without Claude authorization: %d %v %q", code, err, stderr.String())
				}
				if pin, err := s.cliPinnedAccount(); err != nil || pin != "" {
					t.Fatalf("unverified Claude account selected: %q %v", pin, err)
				}
				return
			}
			if err != nil || code != 0 || !strings.Contains(stderr.String(), "在 Cage 浏览器") || !strings.Contains(stderr.String(), "Claude 账号已登录") {
				t.Fatalf("login: %d %v %q", code, err, stderr.String())
			}
			pin, err := s.cliPinnedAccount()
			list, listErr := s.cliAccountRoster()
			if err != nil || listErr != nil || len(list.Accounts) != 1 || list.Accounts[0].ID != pin || list.Accounts[0].Enabled || list.PendingLogin != "" {
				t.Fatalf("new enrollment did not become usable by the CLI: %s %+v %v %v", pin, list, err, listErr)
			}
		})
	}
}

func TestHostCLICancelStopsPipeGuestUnderBackpressure(t *testing.T) {
	s := agentTestController(t)
	s.ready = true
	s.claudeAccounts.probe = func(context.Context, string) (claudeGuestSnapshot, error) {
		return claudeGuestSnapshot{LoggedIn: true, Email: "qa@example.test"}, nil
	}
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.projectShares = map[string]*projectShare{project: {}}
	path := filepath.Join(project, "pid")
	stub := filepath.Join(project, "claude")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho $$ > "+shellescape.Quote(path)+"\nexec sleep 60\n"), 0700); err != nil {
		t.Fatal(err)
	}
	mobileTestSSHRewrite(t, s, project, []string{"HOME=" + project, "PATH=/usr/bin:/bin"}, func(script string) string {
		return strings.Replace(script, "exec claude ", "exec "+shellescape.Quote(stub)+" ", 1)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := agentConnect(ctx, s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	command, interrupt := context.WithCancel(ctx)
	defer interrupt()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = cliExchange(command, client, hostCLIRequest{Action: "run", Directory: project, Args: []string{"-p"}}, io.LimitReader(zeroReader{}, 16<<20), io.Discard, io.Discard, nil)
	}()
	var pid int
	for pid == 0 {
		data, _ := os.ReadFile(path)
		_, _ = fmt.Sscanf(string(data), "%d", &pid)
		select {
		case <-ctx.Done():
			t.Fatal("guest did not start")
		case <-time.After(20 * time.Millisecond):
		}
	}
	interrupt()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("host CLI did not cancel")
	}
	for syscall.Kill(pid, 0) == nil {
		select {
		case <-ctx.Done():
			t.Fatal("disconnected pipe kept guest process alive")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) { clear(b); return len(b), nil }

// This opt-in test uses the built cage executable from a real shell process,
// the official Claude executable and a loopback-only Messages provider.
func TestHostCLIOfficialExecutable(t *testing.T) {
	binary, cli := os.Getenv("CAGE_TEST_CAGE_CLI"), os.Getenv("CAGE_TEST_CLAUDE_CLI")
	if !filepath.IsAbs(binary) || !filepath.IsAbs(cli) {
		t.Skip("set CAGE_TEST_CAGE_CLI and CAGE_TEST_CLAUDE_CLI to absolute executable paths")
	}
	s := agentTestController(t)
	f, err := os.Open(binary)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	_, err = io.Copy(digest, f)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	s.identity.Build = fmt.Sprintf("%x", digest.Sum(nil))
	c, err := openController(s.profile)
	if err != nil {
		t.Fatal(err)
	}
	record := c.record
	c.close()
	record.Controller = s.identity
	if err = writePrivate(filepath.Join(s.profile, "control.json"), record); err != nil {
		t.Fatal(err)
	}
	s.ready = true
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The SSH fixture shares the test filesystem with the test process.
	// Leave the grant absent so the public command must create it.
	s.projectShares = map[string]*projectShare{project: {done: make(chan struct{})}}
	s.claudeAccounts.probe = func(context.Context, string) (claudeGuestSnapshot, error) {
		return claudeGuestSnapshot{LoggedIn: true, Email: "test@example.invalid"}, nil
	}
	home := t.TempDir()
	model := mobileTestModel(t, new(atomic.Bool))
	defer model.Close()
	bin := filepath.Join(home, "bin")
	if err = os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(cli, filepath.Join(bin, "claude")); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(home, ".bash_profile"), []byte("export PATH="+shellescape.Quote(bin)+":/usr/bin:/bin\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// The guest normally verifies the selected subscription in its own
	// directory. Use two distinct IDs here to exercise that launch contract
	// with a loopback provider and without a real subscription.
	sarah := claudeAccount{Number: 3, ID: strings.Repeat("b", 24), ProfileID: strings.Repeat("c", 24),
		Name: "Sarah", Email: "sarah@example.test", Organization: "example", Source: "managed", Status: "ready"}
	helper := "#!/bin/sh\n" +
		"test \"$1\" = native && test \"$2\" = " + sarah.guestID() +
		" && test \"$CAGE_CLAUDE_SUBJECT\" = " + claudeIdentityHash(sarah.Email, sarah.Organization) + " || exit 42\n" +
		"shift 2\nexec claude \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "fixture-account"), []byte(helper), 0700); err != nil {
		t.Fatal(err)
	}
	mobileTestSSHRewrite(t, s, home, []string{"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin", "CLAUDE_CONFIG_DIR=" + filepath.Join(home, "config"),
		"ANTHROPIC_API_KEY=sk-ant-local-fixture", "ANTHROPIC_BASE_URL=" + model.URL, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1",
		"HTTPS_PROXY=http://127.0.0.1:1", "HTTP_PROXY=http://127.0.0.1:1", "NO_PROXY=127.0.0.1,localhost", "LANG=en_US.UTF-8"}, func(command string) string {
		return strings.ReplaceAll(command, "/usr/local/bin/cage-claude-account", "fixture-account")
	})
	call := func(stdin string, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "CLAUDE_CAGE_CONFIG="+s.configPath, "ANTHROPIC_API_KEY=must-not-be-forwarded", "ANTHROPIC_BASE_URL=http://127.0.0.1:1")
		cmd.Stdin = strings.NewReader(stdin)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		body, err := cmd.Output()
		if err != nil {
			t.Fatalf("cage %q: %v, stderr=%s, stdout=%s", args, err, stderr.String(), body)
		}
		return string(body)
	}
	if got := call("", "claude", "mcp", "--help"); !strings.Contains(got, "claude mcp") {
		t.Fatal(got)
	}
	if projects, err := s.projectPaths(); err != nil || len(projects) != 0 {
		t.Fatalf("help created a project: %v %v", projects, err)
	}
	body := call("follow-up\n", "claude", "-p", "--bare", "--no-session-persistence", "--output-format", "json", "summarize stdin")
	var result struct {
		Result  string
		IsError bool `json:"is_error"`
	}
	if err := json.Unmarshal([]byte(body), &result); err != nil || result.IsError || result.Result != "isolated continuation" {
		t.Fatalf("unexpected JSON: %s", body)
	}
	master, terminalIn, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer terminalIn.Close()
	interactiveInput, finish := context.WithTimeout(context.Background(), 12*time.Second)
	defer finish()
	printCmd := exec.CommandContext(interactiveInput, binary, "claude", "-p", "--bare", "--no-session-persistence", "follow-up from a terminal")
	printCmd.Dir, printCmd.Stdin = project, terminalIn
	printCmd.Env = append(os.Environ(), "CLAUDE_CAGE_CONFIG="+s.configPath)
	if data, err := printCmd.Output(); err != nil || !strings.Contains(string(data), "isolated continuation") {
		t.Fatalf("print with open terminal input: %q %v", data, err)
	}
	if got, err := s.projectPaths(); err != nil || !reflect.DeepEqual(got, []string{project}) {
		t.Fatalf("projects=%v err=%v", got, err)
	}
	stream := call("", "claude", "-p", "--bare", "--no-session-persistence", "--output-format", "stream-json", "--verbose", "follow-up")
	frames := strings.Split(strings.TrimSpace(stream), "\n")
	if len(frames) < 2 {
		t.Fatal(stream)
	}
	for _, frame := range frames {
		if !json.Valid([]byte(frame)) {
			t.Fatalf("invalid streamed event: %q", frame)
		}
	}
	id := uuid.NewString()
	verifySession := func(args ...string) {
		t.Helper()
		body := call("", append([]string{"claude", "-p", "--bare", "--output-format", "json"}, args...)...)
		var reply struct {
			SessionID string `json:"session_id"`
			IsError   bool   `json:"is_error"`
		}
		if err := json.Unmarshal([]byte(body), &reply); err != nil || reply.IsError || reply.SessionID != id {
			t.Fatalf("resume %q: %s", args, body)
		}
	}
	verifySession("--session-id", id, "follow-up one")
	verifySession("--resume", id, "follow-up two")
	verifySession("--continue", "follow-up three")
	other := claudeAccount{ID: strings.Repeat("a", 24), Number: 2, Name: "work", Email: "work@example.test", Source: "managed", Enabled: true, Status: "ready"}
	if err := s.saveClaudeAccounts(claudeAccountList{Accounts: []claudeAccount{other, sarah}}); err != nil {
		t.Fatal(err)
	}
	call("", "account", "use", "work")
	s.noteClaudeSelection(other.ID, project)
	if got := call("", "sarah", "mcp", "--help"); !strings.Contains(got, "claude mcp") {
		t.Fatal(got)
	}
	named := call("follow-up\n", "sarah", "-p", "--bare", "--no-session-persistence", "--output-format", "json", "summarize stdin")
	result.Result = ""
	if err := json.Unmarshal([]byte(named), &result); err != nil || result.IsError || result.Result != "isolated continuation" {
		t.Fatalf("named Claude invocation: %s", named)
	}
	if pin, err := s.cliPinnedAccount(); err != nil || pin != other.ID {
		t.Fatalf("named invocation changed the default: %q, %v", pin, err)
	}
	call("", "account", "use", "auto")
	sarah.Enabled = true
	if err := s.saveClaudeAccounts(claudeAccountList{Accounts: []claudeAccount{other, sarah}}); err != nil {
		t.Fatal(err)
	}
	if next, err := s.cliDefaultAccount(project); err != nil || next.ID != other.ID {
		t.Fatalf("single-use account changed the project's automatic selection: %+v, %v", next, err)
	}
	s.account.lock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	locked := exec.CommandContext(ctx, binary, "sarah", "mcp", "--help")
	locked.Env = append(os.Environ(), "CLAUDE_CAGE_CONFIG="+s.configPath)
	locked.Dir = project
	if data, err := locked.Output(); err == nil || len(data) != 0 {
		t.Fatalf("locked CLI wrote stdout or succeeded: %q %v", data, err)
	}
}

func TestHostCLIDetachedTerminalAcceptsDesktopInput(t *testing.T) {
	t.Run("default", func(t *testing.T) { checkDetachedCLI(t, "") })
	t.Run("named", func(t *testing.T) { checkDetachedCLI(t, "Sarah") })
}

func checkDetachedCLI(t *testing.T, name string) {
	t.Helper()
	s := agentTestController(t)
	s.ready = true
	s.claudeAccounts.probe = func(context.Context, string) (claudeGuestSnapshot, error) {
		return claudeGuestSnapshot{LoggedIn: true, Email: "fixture@example.test"}, nil
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	request := hostCLIRequest{Action: "run", Directory: dir, TTY: true, Cols: 80, Rows: 24}
	var expectedID string
	if name != "" {
		expectedID = strings.Repeat("a", 24)
		a := claudeAccount{ID: expectedID, Name: name, Email: "fixture@example.test", Source: "managed", Status: "ready"}
		if err := s.saveClaudeAccounts(claudeAccountList{Accounts: []claudeAccount{a}}); err != nil {
			t.Fatal(err)
		}
		request.Action, request.Name = "run-by-name", name
	}
	s.projectShares = map[string]*projectShare{dir: {}}
	mobileTestSSHRewrite(t, s, t.TempDir(), []string{"PATH=/usr/bin:/bin"}, func(string) string {
		return "printf 'guest-ready\\n'; while read -r line; do printf 'guest-saw<%s>\\n' \"$line\"; done"
	})
	t.Cleanup(s.terminals.close)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := agentConnect(ctx, s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	dial := func(path string) *websocket.Conn {
		t.Helper()
		u, _ := url.Parse(c.origin)
		headers := http.Header{"Origin": {c.origin}, hostCLIHeader: {hostCLIProof(c.record.Token)}}
		for _, cookie := range c.client.Jar.Cookies(u) {
			headers.Add("Cookie", cookie.String())
		}
		ws, _, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(c.origin, "http")+path, headers)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ws.Close() })
		return ws
	}
	native := dial("/api/host-cli")
	if err := native.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	readTerminalViewerUntil(t, native, "guest-ready")
	s.terminals.mu.Lock()
	var session *terminalSession
	for _, entry := range s.terminals.sessions {
		session = entry
	}
	s.terminals.mu.Unlock()
	if session == nil {
		t.Fatal("native PTY was not visible to the workbench")
	}
	if session.claudeAccountID != expectedID {
		t.Fatalf("workbench PTY is bound to %q; want %q", session.claudeAccountID, expectedID)
	}
	viewer := dial("/api/terminals/" + session.id + "/stream")
	readTerminalViewerUntil(t, viewer, "guest-ready")
	if err := native.WriteMessage(websocket.BinaryMessage, []byte("first\r")); err != nil {
		t.Fatal(err)
	}
	readTerminalViewerUntil(t, viewer, "guest-saw<first>")
	_ = native.Close()
	for {
		session.mu.Lock()
		owned := session.hostInput
		session.mu.Unlock()
		if !owned {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("desktop did not recover control")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := viewer.WriteJSON(map[string]string{"type": "input", "data": "from-desktop\r"}); err != nil {
		t.Fatal(err)
	}
	readTerminalViewerUntil(t, viewer, "guest-saw<from-desktop>")
	s.account.lock()
	_ = viewer.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, _, e := viewer.ReadMessage()
		if e != nil {
			if timeout, ok := e.(net.Error); ok && timeout.Timeout() {
				t.Fatal("viewer was not disconnected on lock")
			}
			break
		}
	}
	if session.ended() {
		t.Fatal("locking the viewer killed the terminal")
	}
}

// Set this only to the SSH configuration of a disposable Cage Debian VM.
// The test uses its real Claude executable and reverse-SSHFS server. All
// host files and the workbench account belong to temporary test directories.
func TestHostCLIDebianPassiveProject(t *testing.T) {
	config := os.Getenv("CAGE_TEST_ISOLATED_SSH_CONFIG")
	if !filepath.IsAbs(config) {
		t.Skip("set CAGE_TEST_ISOLATED_SSH_CONFIG to a disposable Cage VM's SSH configuration")
	}
	s := agentTestController(t)
	s.ready = true
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	var matches int
	for i, line := range lines {
		if strings.HasPrefix(line, "Host lima-") {
			lines[i] = "Host " + s.sshTarget()
			matches++
		}
	}
	if matches != 1 {
		t.Fatal("expected the SSH configuration of one isolated Lima guest")
	}
	if err := os.MkdirAll(s.instanceDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.instanceDir(), "ssh.config"), []byte(strings.Join(lines, "\n")), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	client, err := agentConnect(ctx, s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	defer s.controlSession.close()
	defer s.closeProjectShares()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, sibling := filepath.Join(root, "shared"), filepath.Join(root, "unshared.txt")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sibling, []byte("host only"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(project, "outside")
	if err := os.Symlink(sibling, link); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	run := func(args ...string) {
		t.Helper()
		stdout.Reset()
		stderr.Reset()
		code, err := cliExchange(ctx, client, hostCLIRequest{Action: "run", Args: args, Directory: project}, strings.NewReader(""), &stdout, &stderr, nil)
		if err != nil || code != 0 {
			t.Fatalf("%q: %d %v %s %s", args, code, err, stdout.String(), stderr.String())
		}
	}
	run("--version")
	if !strings.Contains(stdout.String(), "Claude Code") {
		t.Fatal(stdout.String())
	}
	t.Log(strings.TrimSpace(stdout.String()))
	run("mcp", "add", "--scope", "project", "--transport", "http", "cage-cli-test", "http://127.0.0.1:1/test")
	mcp, err := os.ReadFile(filepath.Join(project, ".mcp.json"))
	if err != nil || !json.Valid(mcp) || !bytes.Contains(mcp, []byte("cage-cli-test")) {
		t.Fatalf("Claude's project edit did not reach the host: %s %v", mcp, err)
	}
	if _, err := s.guest(ctx, "mountpoint -q "+shellescape.Quote(project)+" && ! test -r "+shellescape.Quote(sibling)+" && ! cat "+shellescape.Quote(link)+" >/dev/null 2>&1"); err != nil {
		t.Fatal("project boundary check failed:", err)
	}
	if got, err := s.projectPaths(); err != nil || !reflect.DeepEqual(got, []string{project}) {
		t.Fatalf("project grant: %v %v", got, err)
	}
}
