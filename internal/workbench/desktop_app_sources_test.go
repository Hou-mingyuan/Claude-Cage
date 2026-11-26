package workbench

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/hostbridge"
)

func TestDesktopInstallHostsRequireReviewedPluginContents(t *testing.T) {
	dir := t.TempDir()
	manifestJSON := `{"name":"antigravity-app","version":"0.1.1","description":"fixture","engines":{"cage":">=0.2.1"},"kind":"guest","permissions":[],"contributes":{"desktopApps":[{"id":"antigravity","title":"App","source":{"type":"archive","resolve":["resolve.py"],"hosts":["dl.example.org"]},"command":["app"]}]}}`
	program := "print('reviewed resolver')\n"
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifestJSON), 0600); err != nil {
		t.Fatal(err)
	}
	resolver := filepath.Join(dir, "resolve.py")
	if err := os.WriteFile(resolver, []byte(program), 0600); err != nil {
		t.Fatal(err)
	}
	manifest, err := hostbridge.LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	target := desktopAppTarget{plugin: manifest.Name, dir: dir, app: manifest.Contributes.DesktopApps[0], id: desktopAppInstance(manifest.Name, "antigravity")}
	review := reviewedDesktopApps[target.plugin]
	if plan, err := prepareDesktopAppInstall(target, review); err != nil || len(plan.httpsHosts) != 0 || plan.unreviewedPlugin != target.plugin {
		t.Fatalf("unreviewed plugin with the official name received %+v (%v)", plan, err)
	}
	digest, err := hostbridge.PluginDigest(dir, target.plugin)
	if err != nil {
		t.Fatal(err)
	}
	review.digests = []string{digest}
	// An earlier lookup may have seen a different manifest. The installer must
	// use the same snapshot as the grant, including arguments and resolver.
	target.app.Command = []string{"stale-command"}
	target.app.Source.Resolve = []string{"stale.py"}
	plan, err := prepareDesktopAppInstall(target, review)
	if err != nil || !slices.Equal(plan.httpsHosts, review.httpsHosts) || plan.unreviewedPlugin != "" || plan.allowedAPTAppID != "" ||
		strings.Contains(plan.script, "stale-command") || !strings.Contains(plan.script, base64.StdEncoding.EncodeToString([]byte(program))) {
		t.Fatalf("reviewed snapshot did not supply the script and hosts: %+v (%v)", plan, err)
	}
	target.app = manifest.Contributes.DesktopApps[0]
	if err := os.WriteFile(resolver, []byte("print('edited')\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if plan, err := prepareDesktopAppInstall(target, review); err != nil || len(plan.httpsHosts) != 0 || plan.unreviewedPlugin != target.plugin {
		t.Fatalf("edited resolver received %+v (%v)", plan, err)
	}
}

func TestDesktopInstallPlanAllowsOwnAPTSource(t *testing.T) {
	dir := t.TempDir()
	manifestJSON := `{"name":"codex-app","version":"0.1.1","description":"fixture","engines":{"cage":">=0.2.1"},"kind":"guest","permissions":[],"contributes":{"desktopApps":[{"id":"codex","title":"Codex","source":{"type":"apt","repository":"https://persistent.oaistatic.com/codex-app-prod/linux/deb","suite":"stable","components":["main"],"key":"https://persistent.oaistatic.com/codex-app-prod/linux/repository-signing-key.gpg","fingerprint":"3BFA0E4AE8B8CC16A2D9BA684A3B4A566C4660E4","package":"chatgpt"},"command":["/usr/bin/chatgpt"]}]}}`
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifestJSON), 0600); err != nil {
		t.Fatal(err)
	}
	manifest, err := hostbridge.LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	target := desktopAppTarget{plugin: manifest.Name, dir: dir, app: manifest.Contributes.DesktopApps[0], id: desktopAppInstance(manifest.Name, "codex")}
	digest, err := hostbridge.PluginDigest(dir, target.plugin)
	if err != nil {
		t.Fatal(err)
	}
	review := reviewedDesktopApps[target.plugin]
	review.digests = []string{digest}
	plan, err := prepareDesktopAppInstall(target, review)
	if err != nil || plan.allowedAPTAppID != target.id || !slices.Equal(plan.httpsHosts, review.httpsHosts) || plan.unreviewedPlugin != "" {
		t.Fatalf("unexpected APT install plan: %+v (%v)", plan, err)
	}
}

func TestDesktopMaintenanceProxyReportsBlockedDestinations(t *testing.T) {
	var rejected []string
	p := maintenanceProxy{
		token: "fixture",
		ctx:   context.Background(),
		onReject: func(destination string) {
			rejected = append(rejected, destination)
		},
	}
	for _, target := range []string{"persistent.oaistatic.com:443", "persistent.oaistatic.com:443", "storage.googleapis.com:443"} {
		req := httptest.NewRequest(http.MethodConnect, target, nil)
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("cage-install:fixture")))
		w := httptest.NewRecorder()
		p.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: expected 403, got %d", target, w.Code)
		}
	}
	want := []string{"persistent.oaistatic.com:443", "storage.googleapis.com:443"}
	if !slices.Equal(rejected, want) || !slices.Equal(p.blockedDestinations(), want) {
		t.Fatalf("rejected=%v blocked=%v want=%v", rejected, p.blockedDestinations(), want)
	}
	cause := errors.New("exit status 22")
	err := maintenanceProxyFailure("安装或更新环境软件", cause, p.blockedDestinations(), "codex-app", "curl: (22) The requested URL returned error: 403")
	if err == nil || !strings.Contains(err.Error(), "未经当前版本 Cage 审核") || !strings.Contains(err.Error(), "persistent.oaistatic.com:443") {
		t.Fatalf("unexpected unreviewed failure: %v", err)
	}
	err = maintenanceProxyFailure("安装或更新环境软件", cause, p.blockedDestinations(), "", "Tunnel connection failed: 403 Forbidden")
	if err == nil || !strings.Contains(err.Error(), "安装代理已拦截未放行的下载源") || !strings.Contains(err.Error(), "storage.googleapis.com:443") {
		t.Fatalf("unexpected policy failure: %v", err)
	}
	if err := maintenanceProxyFailure("安装或更新环境软件", cause, p.blockedDestinations(), "codex-app", "No space left on device"); err != nil {
		t.Fatalf("unrelated failure should not be rewritten as 403 policy rejection: %v", err)
	}
}

func TestDesktopMaintenanceProxyChecksOriginPerRequest(t *testing.T) {
	const codex = "persistent.oaistatic.com"
	const metadata = "antigravity-hub-auto-updater-974169037036.us-central1.run.app"
	const archive = "storage.googleapis.com"
	for _, tc := range []struct {
		plugin, method, address string
		allow                   bool
	}{
		{"codex-app", "CONNECT", codex + ":443", true},
		{"antigravity-app", "CONNECT", metadata + ":443", true},
		{"antigravity-app", "CONNECT", archive + ":443", true},
		{"", "CONNECT", codex + ":443", false},
		{"", "CONNECT", metadata + ":443", false},
		{"codex-app", "CONNECT", archive + ":443", false},
		{"antigravity-app", "CONNECT", codex + ":443", false},
		{"codex-app", "GET", "http://" + codex, false},
		{"antigravity-app", "CONNECT", metadata + ":80", false},
		{"antigravity-app", "CONNECT", archive + ".example.org:443", false},
	} {
		t.Run(tc.plugin+" "+tc.address, func(t *testing.T) {
			dialed := false
			p := maintenanceProxy{token: "fixture", ctx: context.Background(), installHTTPSHosts: reviewedDesktopApps[tc.plugin].httpsHosts, dial: func(_ context.Context, _, address string) (net.Conn, error) {
				dialed = true
				if address != tc.address {
					t.Errorf("unexpected address %s", address)
				}
				return nil, errors.New("offline fixture")
			}}
			req := httptest.NewRequest(tc.method, tc.address, nil)
			req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("cage-install:fixture")))
			w := httptest.NewRecorder()
			p.ServeHTTP(w, req)
			if dialed != tc.allow || (!dialed && w.Code != http.StatusForbidden) {
				t.Fatalf("dialed %v, response %d", dialed, w.Code)
			}
		})
	}
}
