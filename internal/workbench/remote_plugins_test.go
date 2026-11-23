package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sylvinus/agent-vm/internal/desktop"
	"github.com/sylvinus/agent-vm/internal/hostbridge"
	"github.com/sylvinus/agent-vm/internal/remotehost"
)

type originCookieTransport struct {
	base   http.RoundTripper
	jar    http.CookieJar
	origin string
}

func (t *originCookieTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	req := r.Clone(r.Context())
	req.Header = r.Header.Clone()
	req.Header.Set("Origin", t.origin)
	for _, c := range t.jar.Cookies(req.URL) {
		req.AddCookie(c)
	}
	res, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	t.jar.SetCookies(req.URL, res.Cookies())
	return res, nil
}

// startTestRemoteSession spins up a real HTTP server for remote and performs
// the exact /api/launch (remote_desktop=true) + /api/session handshake.
func startTestRemoteSession(t *testing.T, remote *Server, remoteDesktop bool) (*remotehost.Session, context.CancelFunc) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote.handler().ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	remote.origin = ts.URL

	jar, _ := cookiejar.New(nil)
	transport := &originCookieTransport{base: ts.Client().Transport, jar: jar, origin: ts.URL}
	client := &http.Client{Transport: transport}

	launchBody, _ := json.Marshal(map[string]any{"token": remote.token, "remote_desktop": remoteDesktop})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/launch", bytes.NewReader(launchBody))
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("launch failed: err=%v status=%v", err, res)
	}
	var launched launchResponse
	if err := json.NewDecoder(res.Body).Decode(&launched); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	parsed, _ := url.Parse(launched.URL)

	sessBody, _ := json.Marshal(map[string]string{"token": parsed.Fragment})
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/session", bytes.NewReader(sessBody))
	req.Header.Set("Content-Type", "application/json")
	res, err = client.Do(req)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("session failed: err=%v status=%v", err, res)
	}
	res.Body.Close()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	session := &remotehost.Session{
		Origin:    ts.URL,
		Client:    client,
		Transport: transport,
		Context:   ctx,
		Manifest: remotehost.PublicManifest{
			Platform:   runtime.GOOS,
			Runtime:    "container",
			InstanceID: strings.Repeat("b", 64),
			Capabilities: map[string]bool{
				"remote_plugins":       true,
				"remote_approvals":     true,
				"client_plugin_bridge": true,
			},
		},
	}
	return session, cancel
}

func writeRemoteTestPlugin(t *testing.T, root, name, version, kind string, permissions []string, platforms []string, script string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{
		"name":        name,
		"displayName": name,
		"version":     version,
		"description": "Test plugin " + name,
		"engines":     map[string]string{"cage": ">=0.1.0"},
		"kind":        kind,
		"permissions": permissions,
		"contributes": map[string]any{
			"mcpServers": []map[string]any{
				{"id": "main", "command": []string{"python3", "server.py"}},
			},
		},
	}
	if len(platforms) > 0 {
		manifest["platforms"] = platforms
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, hostbridge.ManifestFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if script == "" {
		script = "print('noop')\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "server.py"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteDesktopTicketAndContainerPluginControlBoundary(t *testing.T) {
	remote := testServer(t)
	// Configure container mode to prove container environments now allow plugins.
	if err := os.WriteFile(filepath.Join(remote.profile, "container.json"), []byte(`{"engine":"docker","image":"sha256:0000"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Regular browser ticket (remote_desktop=false) must NOT control host tools or sync.
	browserSession, cancelBrowser := startTestRemoteSession(t, remote, false)
	defer cancelBrowser()
	req, _ := http.NewRequest(http.MethodPost, browserSession.Origin+"/api/host-tools", strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	res, err := browserSession.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("browser ticket mutated host tools: %d", res.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, browserSession.Origin+"/api/host-tools/sync", nil)
	res, err = browserSession.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("browser ticket read plugin sync manifest: %d", res.StatusCode)
	}

	// SSH remote-desktop ticket CAN inspect and toggle host tools in container mode.
	sshSession, cancelSSH := startTestRemoteSession(t, remote, true)
	defer cancelSSH()
	req, _ = http.NewRequest(http.MethodGet, sshSession.Origin+"/api/host-tools", nil)
	res, err = sshSession.Client.Do(req)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("get host-tools failed: err=%v status=%v", err, res)
	}
	var view hostToolsView
	if err := json.NewDecoder(res.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if !view.Available || !view.Headless {
		t.Fatalf("expected container host tools available and headless, got %+v", view)
	}
	req, _ = http.NewRequest(http.MethodPost, sshSession.Origin+"/api/host-tools", strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	res, err = sshSession.Client.Do(req)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("remote-desktop failed to enable container host tools: err=%v status=%v", err, res)
	}
	res.Body.Close()
}

func TestRemoteApprovalChannelRoundTripAndSecurity(t *testing.T) {
	remote := testServer(t)
	session, cancel := startTestRemoteSession(t, remote, true)
	defer cancel()

	var mu sync.Mutex
	var prompts []string
	nextScope := desktop.Once
	nextAnswered := true

	clientCtx, stopClient := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runRemoteApprovalClient(clientCtx, session, "prod-vps", func(ctx context.Context, title, msg string) (desktop.Scope, bool) {
			mu.Lock()
			prompts = append(prompts, title+"\n"+msg)
			scope, answered := nextScope, nextAnswered
			mu.Unlock()
			return scope, answered
		})
	}()
	defer func() {
		stopClient()
		<-done
	}()

	deadline := time.Now().Add(5 * time.Second)
	for !remote.remoteApprovals.connected() {
		if time.Now().After(deadline) {
			t.Fatal("remote approval channel did not connect")
		}
		time.Sleep(15 * time.Millisecond)
	}

	approvals := newHostApprovals()
	approvals.ask = func(context.Context, string, string) (desktop.Scope, bool) {
		t.Error("local fallback dialog should not run while remote approval client is connected")
		return desktop.Deny, false
	}
	approvals.remoteAsk = remote.remoteApprovals.ask
	approvals.grants = remote.hostGrantStore()
	approvals.permissions = map[string][]string{"ops-cli": {"commands"}}

	// 1. Command approval: Once allows once and includes remote host name in client dialog.
	if err := approvals.approve(context.Background(), hostbridge.CommandRequest{Command: "git status", Dir: "/srv/app", Project: "/srv/app"}); err != nil {
		t.Fatalf("expected command once approval to succeed: %v", err)
	}
	mu.Lock()
	if len(prompts) != 1 || !strings.Contains(prompts[0], "prod-vps") || !strings.Contains(prompts[0], "git status") {
		t.Fatalf("unexpected client prompt: %v", prompts)
	}
	nextScope = desktop.Always
	mu.Unlock()

	// 2. Tool approval: Always persists grant so second call does not prompt again.
	if err := approvals.approveTool(context.Background(), hostbridge.ToolRequest{Plugin: "ops-cli", Tool: "deploy", Arguments: `{"env":"staging"}`}); err != nil {
		t.Fatalf("expected tool always approval to succeed: %v", err)
	}
	if err := approvals.approveTool(context.Background(), hostbridge.ToolRequest{Plugin: "ops-cli", Tool: "deploy", Arguments: `{"env":"staging"}`}); err != nil {
		t.Fatalf("expected second tool call to use persisted grant: %v", err)
	}
	mu.Lock()
	if len(prompts) != 2 {
		t.Fatalf("expected persisted grant to avoid third dialog, got %d dialogs", len(prompts))
	}
	nextScope = desktop.Deny
	mu.Unlock()

	// 3. Deny refuses execution.
	if err := approvals.approve(context.Background(), hostbridge.CommandRequest{Command: "rm -rf /tmp/x", Dir: "/srv/app", Project: "/srv/app"}); err == nil {
		t.Fatal("expected denied command to fail")
	}

	// 4. Client-side validation rejects invisible/control characters even if forged.
	if err := validateRemoteApprovalPrompt(remoteApprovalPrompt{Kind: "command", Command: "echo \u202eok", Dir: "/srv/app"}); err == nil {
		t.Fatal("expected bidi control character to be rejected on client side")
	}
}

func TestRemotePluginAutoSyncIncrementalAndDataPreservation(t *testing.T) {
	remote := testServer(t)
	// Container mode makes remote headless.
	if err := os.WriteFile(filepath.Join(remote.profile, "container.json"), []byte(`{"engine":"docker","image":"sha256:0000"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Pre-populate a remote-only plugin plus existing grants and settings that must survive sync.
	writeRemoteTestPlugin(t, remote.hostPluginDir(), "remote-only", "1.0.0", hostbridge.KindGuest, []string{}, nil, "")
	writeRemoteTestPlugin(t, remote.hostPluginDir(), "guest-ok", "1.0.0", hostbridge.KindGuest, []string{}, nil, "print('v1')\n")
	if err := remote.hostGrantStore().grant(hostGrantPlugin+"guest-ok", []string{}); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(remote.profile, "plugin-settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"guest-ok":{"endpoint":"https://remote.internal"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	session, cancel := startTestRemoteSession(t, remote, true)
	defer cancel()

	localPlugins := filepath.Join(t.TempDir(), "plugins")
	writeRemoteTestPlugin(t, localPlugins, "guest-ok", "1.2.0", hostbridge.KindGuest, []string{}, nil, "print('v2')\n")
	writeRemoteTestPlugin(t, localPlugins, "host-cli", "0.5.0", hostbridge.KindHost, []string{"commands"}, []string{runtime.GOOS}, "print('cli')\n")
	writeRemoteTestPlugin(t, localPlugins, "host-gui", "0.5.0", hostbridge.KindHost, []string{"screen"}, []string{runtime.GOOS}, "print('gui')\n")

	runner := newClientPluginSyncRunner(session, localPlugins)
	defer runner.close()

	deadline := time.Now().Add(8 * time.Second)
	var st clientPluginSyncStatus
	for {
		st = runner.snapshot()
		if st.State == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sync did not complete: %+v", st)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(st.Failed) != 0 {
		t.Fatalf("unexpected sync failures: %+v", st.Failed)
	}
	if len(st.Synced) != 2 {
		t.Fatalf("expected 2 synced plugins (guest-ok, host-cli), got %+v", st.Synced)
	}
	if len(st.Skipped) != 1 || st.Skipped[0].Name != "host-gui" {
		t.Fatalf("expected host-gui skipped on headless remote, got %+v", st.Skipped)
	}

	// Verify remote-only plugin was preserved (incremental, non-destructive).
	if _, err := hostbridge.LoadManifest(filepath.Join(remote.hostPluginDir(), "remote-only")); err != nil {
		t.Fatalf("remote-only plugin should be preserved: %v", err)
	}
	// Verify guest-ok upgraded to 1.2.0 and old copy moved to plugins-removed.
	upgraded, err := hostbridge.LoadManifest(filepath.Join(remote.hostPluginDir(), "guest-ok"))
	if err != nil || upgraded.Version != "1.2.0" {
		t.Fatalf("guest-ok not upgraded: %+v %v", upgraded, err)
	}
	removedEntries, err := os.ReadDir(filepath.Join(remote.profile, "plugins-removed"))
	if err != nil || len(removedEntries) == 0 {
		t.Fatalf("expected old plugin version preserved in plugins-removed: %v %v", removedEntries, err)
	}
	// Verify grants and settings were untouched.
	if !remote.hostGrantStore().allowed(hostGrantPlugin+"guest-ok", []string{}) {
		t.Fatal("existing remote plugin grant was lost during sync")
	}
	rawSettings, _ := os.ReadFile(settingsPath)
	if !strings.Contains(string(rawSettings), "https://remote.internal") {
		t.Fatalf("existing remote plugin settings were overwritten: %s", rawSettings)
	}
}

func TestRemoteClientPluginReverseBridgeEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is unavailable")
	}
	remote := testServer(t)
	token, err := hostbridge.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	remoteCmdDir := t.TempDir()
	bridge, err := hostbridge.Start(token, hostbridge.Command{Dirs: func() []string { return []string{remoteCmdDir} }})
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	remote.hostTools = &hostTools{bridge: bridge, serving: true, done: make(chan struct{}), cancel: func() {}}

	session, cancel := startTestRemoteSession(t, remote, true)
	defer cancel()

	localProfile := t.TempDir()
	localPlugins := filepath.Join(localProfile, "plugins")
	echoServer := `import json, sys
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    msg = json.loads(line)
    mid = msg.get("id")
    method = msg.get("method")
    if method == "initialize":
        resp = {"jsonrpc": "2.0", "id": mid, "result": {"protocolVersion": "2025-03-26", "capabilities": {"tools": {}}, "serverInfo": {"name": "echo", "version": "1"}}}
    elif method == "notifications/initialized":
        continue
    elif method == "tools/list":
        resp = {"jsonrpc": "2.0", "id": mid, "result": {"tools": [{"name": "local_echo", "description": "Echo text on this machine", "inputSchema": {"type": "object", "properties": {"text": {"type": "string"}}}}]}}
    elif method == "tools/call":
        args = msg.get("params", {}).get("arguments") or {}
        resp = {"jsonrpc": "2.0", "id": mid, "result": {"content": [{"type": "text", "text": "from-local:" + str(args.get("text", ""))}]}}
    else:
        resp = {"jsonrpc": "2.0", "id": mid, "error": {"code": -32601, "message": "not found"}}
    sys.stdout.write(json.dumps(resp) + "\n")
    sys.stdout.flush()
`
	writeRemoteTestPlugin(t, localPlugins, "local-echo", "1.0.0", hostbridge.KindHost, []string{}, []string{runtime.GOOS}, echoServer)

	var seenDialog string
	mgr := newClientPluginBridgeManager(session, "dev-vps", localProfile, func(ctx context.Context, title, msg string) (desktop.Scope, bool) {
		seenDialog = title + "\n" + msg
		return desktop.Once, true
	})
	defer mgr.close()

	st, err := mgr.setShared(context.Background(), "local-echo", true)
	if err != nil {
		t.Fatalf("setShared(true) failed: %v", err)
	}
	if !st.Connected {
		t.Fatalf("expected bridge connected, got %+v", st)
	}

	// Connect an MCP client to the REMOTE bridge (simulating Claude inside the remote container).
	ctx, stopMCP := context.WithTimeout(context.Background(), 15*time.Second)
	defer stopMCP()
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "claude-sim", Version: "1"}, nil)
	httpClient := &http.Client{Transport: testBearerTransport{token: token}}
	mcpSession, err := mcpClient.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   "http://" + bridge.Addr(),
		HTTPClient: httpClient,
		MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mcpSession.Close()

	// Wait briefly for the register frame to land on the remote bridge.
	var found bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		listed, err := mcpSession.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range listed.Tools {
			if tool.Name == "local_echo" {
				found = true
				break
			}
		}
		if found {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !found {
		t.Fatal("reverse-bridged tool local_echo did not appear on remote MCP bridge")
	}

	// Call the tool from remote Claude -> traverses tunnel -> approves & runs on local client -> returns result.
	res, err := mcpSession.CallTool(ctx, &mcp.CallToolParams{Name: "local_echo", Arguments: map[string]any{"text": "hello-remote"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || len(res.Content) == 0 {
		t.Fatalf("unexpected tool result: %+v", res)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok || text.Text != "from-local:hello-remote" {
		t.Fatalf("unexpected text content: %+v", res.Content[0])
	}
	if !strings.Contains(seenDialog, "dev-vps") || !strings.Contains(seenDialog, "local-echo") {
		t.Fatalf("local approval dialog missing remote host/plugin context: %q", seenDialog)
	}

	// Unshare -> remote bridge immediately withdraws local_echo.
	if _, err := mgr.setShared(context.Background(), "local-echo", false); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		listed, err := mcpSession.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		stillPresent := false
		for _, tool := range listed.Tools {
			if tool.Name == "local_echo" {
				stillPresent = true
			}
		}
		if !stillPresent {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("local_echo remained on remote bridge after unsharing")
}

type testBearerTransport struct{ token string }

func (t testBearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	req := r.Clone(r.Context())
	req.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(req)
}

func TestRemotePluginViewGatewayProxyEnforcesStrictCSPAndBlocksInternalRoutes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "evil=1")
		w.Header().Set("Content-Security-Policy", "default-src *")
		w.Header().Set("X-Frame-Options", "ALLOWALL")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><script type='module' src='app.js'></script>"))
	}))
	defer upstream.Close()

	s := testServer(t)
	s.remote = newRemoteGateway(s.profile)
	t.Cleanup(s.remote.close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := &remotehost.Session{
		Origin:    upstream.URL,
		Client:    upstream.Client(),
		Transport: upstream.Client().Transport,
		Context:   ctx,
		Manifest:  remotehost.PublicManifest{Platform: "linux", InstanceID: strings.Repeat("c", 64)},
	}
	// Bind active remote session on a desktop gateway for testing routeRemotePluginView.
	s.remote = newDesktopGateway(s.profile, &gatewayLocalFixture{endpoint: upstream})
	t.Cleanup(s.remote.close)
	d := s.remote.desktop
	d.mu.Lock()
	d.active = &desktopTarget{id: "conn-remote-1", mode: "remote", ctx: ctx, cancel: cancel, session: session}
	d.generation = "conn-remote-1"
	d.mu.Unlock()
	// Also attach via test helper on manager-less target: acquire uses local session when remote==nil.
	d.active.remote = nil

	viewPath := "/plugin-views/demo/" + strings.Repeat("a", 32) + "/main/"
	r := httptest.NewRequest(http.MethodGet, s.origin+viewPath, nil)
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected proxied plugin view 200, got %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Set-Cookie") != "" {
		t.Fatal("remote Set-Cookie leaked through plugin view proxy")
	}
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "sandbox allow-scripts") || !strings.Contains(csp, "connect-src 'none'") || strings.Contains(csp, "default-src *") {
		t.Fatalf("plugin view proxy CSP weakened: %q", csp)
	}

	// Browser cannot directly hit internal remote upgrade channels through gateway.
	for _, internalPath := range []string{"/api/host-tools/approvals/channel", "/api/host-tools/client-bridge"} {
		req := httptest.NewRequest(http.MethodPost, s.origin+internalPath, strings.NewReader("{}"))
		req.Header.Set("Origin", s.origin)
		req.Header.Set(connectionHeader, "conn-remote-1")
		req.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for browser access to %s, got %d", internalPath, rec.Code)
		}
	}

	// Untrusted local browser (no -desktop cookie) cannot POST /api/host-tools to remote.
	req := httptest.NewRequest(http.MethodPost, s.origin+"/api/host-tools", strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Origin", s.origin)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(connectionHeader, "conn-remote-1")
	req.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for untrusted browser mutating remote host-tools, got %d", rec.Code)
	}
}
