package workbench

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sylvinus/agent-vm/internal/desktop"
	"github.com/sylvinus/agent-vm/internal/hostbridge"
)

func TestHostApprovalsNeverAlwaysEnforcesPerCallConfirmation(t *testing.T) {
	grants := (&Server{profile: t.TempDir()}).hostGrantStore()
	if err := grants.grant(hostGrantPlugin+workspaceControlPlugin, []string{}); err != nil {
		t.Fatal(err)
	}

	asks := 0
	approvals := newHostApprovals()
	approvals.grants = grants
	approvals.permissions = map[string][]string{workspaceControlPlugin: {}}
	approvals.neverAlways[workspaceControlPlugin+"/"+workspaceControlManageTool] = true
	approvals.ask = func(ctx context.Context, title, msg string) (desktop.Scope, bool) {
		asks++
		// Even if the native dialog returns Always, forceAsk must treat it as Once.
		return desktop.Always, true
	}

	// Regular tool respects the plugin-level always-allow grant.
	if err := approvals.approveTool(context.Background(), hostbridge.ToolRequest{
		Plugin:    workspaceControlPlugin,
		Tool:      "workspace_get_status",
		Arguments: "{}",
	}); err != nil {
		t.Fatalf("status tool approval: %v", err)
	}
	if asks != 0 {
		t.Fatalf("expected 0 prompts for always-allowed status tool, got %d", asks)
	}

	// Sensitive tool ALWAYS prompts every time despite the plugin-level grant.
	for want := 1; want <= 2; want++ {
		if err := approvals.approveTool(context.Background(), hostbridge.ToolRequest{
			Plugin:    workspaceControlPlugin,
			Tool:      workspaceControlManageTool,
			Arguments: `{"action":"reload"}`,
		}); err != nil {
			t.Fatalf("manage tool call %d: %v", want, err)
		}
		if asks != want {
			t.Fatalf("expected %d prompts for neverAlways tool, got %d", want, asks)
		}
	}
}

func TestWorkspaceControlBridgeSecurityAndEndToEndMCP(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}

	profile := t.TempDir()
	s := &Server{
		profile:            profile,
		token:              randomToken(),
		origin:             "http://localhost:19010",
		draftStoreOverride: t.TempDir(),
		preferences: Preferences{
			AutoTimezone:     true,
			AutoLanguage:     true,
			BalancedDisplay:  true,
			ProjectFileCache: true,
		},
	}
	s.ensureDefaultPlugins()

	bridge, err := s.startWorkspaceControlBridge()
	if err != nil {
		t.Fatalf("startWorkspaceControlBridge: %v", err)
	}
	defer bridge.Close()

	// Unauthenticated and wrong-token requests must be rejected.
	resp, err := http.Get(bridge.URL() + "/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d want 401", resp.StatusCode)
	}

	callBridge := func(method, path, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, bridge.URL()+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+bridge.Token())
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	// Sensitive/unknown preference fields must be strictly rejected.
	for _, badBody := range []string{
		`{"host_tools":true}`,
		`{"workspace":"/etc"}`,
		`{"host_plugins_off":""}`,
		`{"browser_home_url":"javascript:alert(1)"}`,
	} {
		res := callBridge(http.MethodPost, "/preferences", badBody)
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 for %s, got %d", badBody, res.StatusCode)
		}
	}

	// Built-in commands plugin cannot be modified via /extensions.
	res := callBridge(http.MethodPost, "/extensions", `{"action":"disable","name":"commands"}`)
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 disabling commands, got %d", res.StatusCode)
	}

	// Non-core extension can be disabled and re-enabled (using its neutral alias).
	res = callBridge(http.MethodPost, "/extensions", `{"action":"disable","name":"plugin-studio"}`)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 disabling plugin-studio, got %d", res.StatusCode)
	}
	if !strings.Contains(s.preferences.HostPluginsOff, "cage-plugin") {
		t.Fatalf("expected cage-plugin disabled in preferences: %q", s.preferences.HostPluginsOff)
	}
	res = callBridge(http.MethodPost, "/extensions", `{"action":"enable","name":"plugin-studio"}`)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 enabling plugin-studio, got %d", res.StatusCode)
	}

	// End-to-end: launch the deployed workspace-control MCP server with scoped env.
	ctrlDir := filepath.Join(s.hostPluginDir(), workspaceControlPlugin)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, python, filepath.Join(ctrlDir, "server.py"))
	cmd.Dir = ctrlDir
	cmd.Env = append(os.Environ(),
		"WORKSPACE_CONTROL_URL="+bridge.URL(),
		"WORKSPACE_CONTROL_TOKEN="+bridge.Token(),
	)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: 2 * time.Second}, nil)
	if err != nil {
		t.Fatalf("connect workspace-control MCP: %v", err)
	}
	defer session.Close()

	callMCP := func(name string, args map[string]any) (string, bool) {
		t.Helper()
		r, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("CallTool %s: %v", name, err)
		}
		var sb strings.Builder
		for _, c := range r.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				sb.WriteString(tc.Text)
			}
		}
		return sb.String(), r.IsError
	}

	// Update appearance via MCP and verify persistence.
	out, isErr := callMCP("workspace_update_appearance", map[string]any{
		"mode":           "dark",
		"ui_font_size":   16,
		"code_font_size": 15,
		"dark_accent":    "#60a5fa",
	})
	if isErr {
		t.Fatalf("workspace_update_appearance error: %s", out)
	}
	savedAppearance := s.readSavedAppearance()
	if savedAppearance.Mode != "dark" || savedAppearance.UIFontSize != 16 || savedAppearance.Dark.Accent != "#60a5fa" {
		t.Fatalf("appearance not persisted: %+v", savedAppearance)
	}

	// Update preferences via MCP and verify persistence.
	out, isErr = callMCP("workspace_update_preferences", map[string]any{
		"clock_12_hour":    true,
		"balanced_display": false,
		"browser_home_url": "https://example.com/",
	})
	if isErr {
		t.Fatalf("workspace_update_preferences error: %s", out)
	}
	if !s.preferences.Clock12Hour || s.preferences.BalancedDisplay || s.preferences.BrowserHomeURL != "https://example.com/" {
		t.Fatalf("preferences not persisted: %+v", s.preferences)
	}

	// Read status via MCP and confirm neutral output reflects updates.
	statusOut, isErr := callMCP("workspace_get_status", map[string]any{})
	if isErr {
		t.Fatalf("workspace_get_status error: %s", statusOut)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(statusOut), &parsed); err != nil {
		t.Fatalf("decode status JSON: %v\n%s", err, statusOut)
	}
	if word, found := hostbridge.CheckWording(&mcp.Tool{Name: "status", Description: statusOut}); found {
		t.Fatalf("workspace_get_status leaked reserved term %q: %s", word, statusOut)
	}
}
