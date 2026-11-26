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
	if _, hosts, err := prepareDesktopAppInstall(target, review); err != nil || len(hosts) != 0 {
		t.Fatalf("unreviewed plugin with the official name received %v (%v)", hosts, err)
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
	script, hosts, err := prepareDesktopAppInstall(target, review)
	if err != nil || !slices.Equal(hosts, review.httpsHosts) || strings.Contains(script, "stale-command") || !strings.Contains(script, base64.StdEncoding.EncodeToString([]byte(program))) {
		t.Fatalf("reviewed snapshot did not supply the script and hosts: %v (%v)", hosts, err)
	}
	target.app = manifest.Contributes.DesktopApps[0]
	if err := os.WriteFile(resolver, []byte("print('edited')\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, hosts, err := prepareDesktopAppInstall(target, review); err != nil || len(hosts) != 0 {
		t.Fatalf("edited resolver received %v (%v)", hosts, err)
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
