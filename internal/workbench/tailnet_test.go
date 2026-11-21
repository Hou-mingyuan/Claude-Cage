package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sylvinus/agent-vm/internal/hostbridge"
)

func TestTailnetAuthorizationURL(t *testing.T) {
	for _, u := range []string{"https://login.tailscale.com/a/abc", "https://login.tailscale.com/c/abc"} {
		if !validTailnetAuthURL(u) {
			t.Fatal(u)
		}
	}
	for _, u := range []string{"http://login.tailscale.com/a/abc", "https://login.tailscale.com.evil.test/a/abc", "https://evil.test/a/abc", "https://user@login.tailscale.com/a/abc", "https://login.tailscale.com:444/a/abc", "javascript:alert(1)", "https://login.tailscale.com/admin"} {
		if validTailnetAuthURL(u) {
			t.Fatal(u)
		}
	}
}
func TestTailnetBoundaries(t *testing.T) {
	s := testServer(t)
	if w := request(s, "POST", "/api/tailscale/connect", "https://evil.test", "{}", true); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := request(s, "GET", "/api/tailscale", "", "", false); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request(s, "GET", "/api/tailscale", "", "", true); w.Code != 409 {
		t.Fatal(w.Code)
	}
}

// The workbench's actions go to the plugin's tools; what a tool reports,
// state or failure, reaches the panel as before.
func TestTailnetActionsGoToThePlugin(t *testing.T) {
	s := testServer(t)
	s.ready = true
	var called []string
	s.tailnetTool = func(_ context.Context, tool string, arguments json.RawMessage) (*mcp.CallToolResult, error) {
		called = append(called, tool+" "+string(arguments))
		result := &mcp.CallToolResult{}
		switch tool {
		case "add_key":
			result.IsError = true
			result.Content = []mcp.Content{&mcp.TextContent{Text: "请粘贴一条有效的 SSH 公钥"}}
		case "open_service":
			result.Content = []mcp.Content{&mcp.TextContent{Text: `{"installed":true,"backend":"Running","forwards":[{"host":"100.88.2.4","port":8080,"address":"127.0.0.1:32123"}]}`}}
		case "logout":
			return nil, &hostbridge.RefusedError{Reason: errors.New("no")}
		case "disconnect":
			result.Content = []mcp.Content{&mcp.TextContent{Text: `{"installed":true,"backend":"NeedsLogin","auth_url":"https://evil.test/a/x"}`}}
		default:
			result.Content = []mcp.Content{&mcp.TextContent{Text: `{"installed":true,"backend":"NeedsLogin","auth_url":"https://login.tailscale.com/a/test"}`}}
		}
		return result, nil
	}
	if w := request(s, "POST", "/api/tailscale/connect", s.origin, "{}", true); w.Code != 200 || !strings.Contains(w.Body.String(), "login.tailscale.com") {
		t.Fatalf("connect: %d %s", w.Code, w.Body.String())
	}
	if w := request(s, "POST", "/api/tailscale/add-key", s.origin, `{"key":"invalid"}`, true); w.Code != 502 || !strings.Contains(w.Body.String(), "有效的 SSH 公钥") {
		t.Fatalf("key error: %d %s", w.Code, w.Body.String())
	}
	if w := request(s, "POST", "/api/tailscale/open-service", s.origin, `{"host":"100.88.2.4","port":8080}`, true); w.Code != 200 || !strings.Contains(w.Body.String(), "127.0.0.1:32123") {
		t.Fatalf("service: %d %s", w.Code, w.Body.String())
	}
	if w := request(s, "POST", "/api/tailscale/logout", s.origin, "{}", true); w.Code != 403 {
		t.Fatalf("an action not let through: %d %s", w.Code, w.Body.String())
	}
	if w := request(s, "POST", "/api/tailscale/disconnect", s.origin, "{}", true); w.Code != 502 {
		t.Fatalf("a sign-in address from elsewhere was passed on: %d %s", w.Code, w.Body.String())
	}
	want := []string{`connect {"control_url":""}`, `add_key {"key":"invalid"}`, `open_service {"host":"100.88.2.4","port":8080}`, `logout {}`, `disconnect {}`}
	if strings.Join(called, "|") != strings.Join(want, "|") {
		t.Fatalf("called %v", called)
	}
	for _, body := range []string{`{"host":"100.88.2.4","port":0}`, `{"host":"100.88.2.4","port":65536}`, `{"host":"100.88.2.4","port":"80"}`, `{"port":80}`} {
		if w := request(s, "POST", "/api/tailscale/open-service", s.origin, body, true); w.Code != 400 {
			t.Fatalf("bad input accepted: %d %s", w.Code, body)
		}
	}
}

// Until Tailscale is installed the panel is told so; installing registers
// the plugin once and remembers that the user asked for it.
func TestTailnetPluginIsRegisteredOnce(t *testing.T) {
	s := testServer(t)
	s.ready = true
	if w := request(s, "GET", "/api/tailscale", "", "", true); w.Code != 200 || !strings.Contains(w.Body.String(), `"installed":false`) {
		t.Fatalf("not installed: %d %s", w.Code, w.Body.String())
	}
	if changed, err := s.ensureTailnetPlugin(); err != nil || !changed {
		t.Fatalf("register: %t %v", changed, err)
	}
	manifest, err := hostbridge.LoadManifest(s.tailnetPluginDir())
	if err != nil || manifest.Kind != hostbridge.KindGuest || manifest.ServerCommand()[1] != "mcp" {
		t.Fatalf("manifest %+v: %v", manifest, err)
	}
	if !s.hostGrantStore().allowed(hostGrantPlugin+tailnetPlugin, manifest.Permissions) {
		t.Fatal("the user's installing it was not remembered")
	}
	// Taken back by the user, it stays taken back.
	if err := s.hostGrantStore().revoke(hostGrantPlugin + tailnetPlugin); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.ensureTailnetPlugin(); err != nil || changed {
		t.Fatalf("registered again: %t %v", changed, err)
	}
	if s.hostGrantStore().allowed(hostGrantPlugin+tailnetPlugin, manifest.Permissions) {
		t.Fatal("a grant the user took back was given again")
	}
	// Installed but not running: the panel says what to do, not "not installed".
	if w := request(s, "GET", "/api/tailscale", "", "", true); w.Code != 502 {
		t.Fatalf("not running: %d %s", w.Code, w.Body.String())
	}
}

func TestTailnetControlServerPersistenceAndSameOriginAuth(t *testing.T) {
	// Custom Headscale origin allows only same-origin HTTPS auth URLs with a path.
	for _, u := range []string{
		"https://headscale.example.com/register/mkey:abc",
		"https://headscale.example.com:443/oidc/register/123",
	} {
		if !validTailnetAuthURLFor(u, "https://headscale.example.com") {
			t.Fatalf("expected valid custom auth URL: %s", u)
		}
	}
	for _, u := range []string{
		"https://login.tailscale.com/a/abc",
		"http://headscale.example.com/register/mkey:abc",
		"https://headscale.example.com/",
		"https://headscale.example.com:8443/register/mkey:abc",
		"https://headscale.example.com.evil.test/register/mkey:abc",
		"https://user@headscale.example.com/register/mkey:abc",
		"https://headscale.example.com/register/mkey:abc#frag",
	} {
		if validTailnetAuthURLFor(u, "https://headscale.example.com") {
			t.Fatalf("expected invalid custom auth URL: %s", u)
		}
	}

	s := testServer(t)
	if !strings.HasPrefix(s.profile, os.TempDir()) {
		t.Fatalf("profile %q outside temp dir", s.profile)
	}
	// Saving works before the environment starts or Tailscale is installed.
	if w := request(s, "POST", "/api/tailscale/control-server", s.origin, `{"control_url":"https://Headscale.Example.COM:8443/"}`, true); w.Code != 200 || !strings.Contains(w.Body.String(), `"control_url":"https://headscale.example.com:8443"`) {
		t.Fatalf("save before start: %d %s", w.Code, w.Body.String())
	}
	if w := request(s, "POST", "/api/tailscale/control-server", s.origin, `{"control_url":"http://insecure.example"}`, true); w.Code != 400 {
		t.Fatalf("http control URL accepted: %d %s", w.Code, w.Body.String())
	}

	s.ready = true
	var calls []string
	authReply := `{"installed":true,"backend":"NeedsLogin","auth_url":"https://headscale.example.com:8443/register/node1"}`
	s.tailnetTool = func(_ context.Context, tool string, arguments json.RawMessage) (*mcp.CallToolResult, error) {
		calls = append(calls, tool+" "+string(arguments))
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: authReply}}}, nil
	}
	// Host pushes saved control_url on connect and accepts matching same-origin auth URL.
	if w := request(s, "POST", "/api/tailscale/connect", s.origin, "{}", true); w.Code != 200 || !strings.Contains(w.Body.String(), "headscale.example.com:8443/register/node1") {
		t.Fatalf("connect custom server: %d %s", w.Code, w.Body.String())
	}
	// Official login.tailscale.com URL must be rejected while custom Headscale is active.
	authReply = `{"installed":true,"backend":"NeedsLogin","auth_url":"https://login.tailscale.com/a/abc"}`
	if w := request(s, "GET", "/api/tailscale", "", "", true); w.Code != 502 {
		t.Fatalf("mismatched auth URL passed: %d %s", w.Code, w.Body.String())
	}
	// Clearing returns to official service and removes tailscale.json.
	authReply = `{"installed":true,"backend":"Stopped"}`
	if w := request(s, "POST", "/api/tailscale/control-server", s.origin, `{"control_url":""}`, true); w.Code != 200 || strings.Contains(w.Body.String(), "control_url") {
		t.Fatalf("clear control server: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(s.profile, tailnetConfigFile)); !os.IsNotExist(err) {
		t.Fatalf("clearing control server left %s behind: %v", tailnetConfigFile, err)
	}
	wantCalls := []string{
		`connect {"control_url":"https://headscale.example.com:8443"}`,
		`status {}`,
		`set_control_url {"control_url":""}`,
	}
	if strings.Join(calls, "|") != strings.Join(wantCalls, "|") {
		t.Fatalf("calls=%v want=%v", calls, wantCalls)
	}
	// Corrupt tailscale.json must fail closed rather than silently falling back to official.
	if err := os.WriteFile(filepath.Join(s.profile, tailnetConfigFile), []byte(`{"control_url":"http://bad.example"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if w := request(s, "GET", "/api/tailscale", "", "", true); w.Code != 500 {
		t.Fatalf("corrupt tailscale.json did not fail closed: %d %s", w.Code, w.Body.String())
	}
}
