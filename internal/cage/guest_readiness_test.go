package cage

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGuestReadinessWaitsForAuthenticatedBrowser(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python unavailable")
	}
	var browserRequests, proxyRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/browser" {
			t.Errorf("unexpected service request at startup: %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			w.WriteHeader(401)
			return
		}
		switch browserRequests.Add(1) {
		case 1:
			_, _ = fmt.Fprint(w, `{"ready":false}`)
			return
		case 2:
			w.WriteHeader(503)
			return
		}
		_, _ = fmt.Fprint(w, `{"ready":true}`)
	}))
	defer server.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { proxyRequests.Add(1); w.WriteHeader(502) }))
	defer proxy.Close()
	dir := t.TempDir()
	script, config := filepath.Join(dir, "wait.py"), filepath.Join(dir, "browser.json")
	if err := os.WriteFile(script, []byte(guestWaitServices), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte(`{"token":"fixture-token"}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-c", `import runpy,sys;runpy.run_path(sys.argv[1])['wait'](sys.argv[2],sys.argv[3],timeout=10)`, script, config, server.URL+"/browser")
	cmd.Env = append(os.Environ(), "http_proxy="+proxy.URL, "HTTP_PROXY="+proxy.URL, "no_proxy=", "NO_PROXY=")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("readiness: %v %s", err, output)
	}
	if browserRequests.Load() < 3 || proxyRequests.Load() != 0 {
		t.Fatalf("premature or proxied readiness: browser=%d proxy=%d", browserRequests.Load(), proxyRequests.Load())
	}
}

// Set this to the SSH configuration of a newly started, disposable Debian
// guest. The test opens the three optional services through their real guest
// endpoints. It uses no accounts, host mounts or external network.
func TestIdleDebianServicesStartOnFirstConnection(t *testing.T) {
	config := os.Getenv("CAGE_TEST_IDLE_GUEST_SSH_CONFIG")
	if config == "" {
		t.Skip("requires a disposable, idle Cage Debian guest")
	}
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	var target string
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "Host" && strings.HasPrefix(fields[1], "lima-") {
			if target != "" {
				t.Fatal("expected one isolated guest in SSH configuration")
			}
			target = fields[1]
		}
	}
	if target == "" {
		t.Fatal("missing isolated guest in SSH configuration")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", "-F", config, "-o", "BatchMode=yes", "-T", target, "sh", "-se")
	cmd.Stdin = strings.NewReader(`
browser_start=$(systemctl show -p ExecMainStartTimestampMonotonic --value cage-browser.service)
test "$browser_start" -gt 0
for unit in cage-editor cage-webvnc cage-audio; do
  systemctl is-active --quiet "$unit.socket"
  listener_start=$(systemctl show -p ActiveEnterTimestampMonotonic --value "$unit.socket")
  if test "$listener_start" -gt "$browser_start"; then echo "$unit listener was late for restored browser tabs"; exit 1; fi
  test "$(systemctl show -p ActiveState --value "$unit.service")" = inactive
done
curl --noproxy '*' -fSs --max-time 20 http://127.0.0.1:18080/healthz >/dev/null
systemctl is-active --quiet cage-editor.service
test "$(systemctl show -p ActiveState --value cage-webvnc.service)" = inactive
test "$(systemctl show -p ActiveState --value cage-audio.service)" = inactive
curl --noproxy '*' -fSs --max-time 10 http://127.0.0.1:16080/vnc.html >/dev/null
systemctl is-active --quiet cage-webvnc.service
test "$(systemctl show -p ActiveState --value cage-audio.service)" = inactive
timeout 10 pactl --server=unix:/run/cage-audio/native list short sinks | grep -q .
`)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("idle guest activation failed: %v\n%s", err, output)
	}
}
