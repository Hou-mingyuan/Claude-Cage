package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/hostbridge"
)

func TestDesktopAppInstanceIsNeutralAndStable(t *testing.T) {
	id := desktopAppInstance("codex", "codex")
	if !validClaudeID(id) || id != desktopAppInstance("codex", "codex") {
		t.Fatalf("instance %q", id)
	}
	if id == desktopAppInstance("codex", "other") || id == desktopAppInstance("codex-x", "codex") {
		t.Fatal("different applications share an instance")
	}
	// Neither the plugin's name nor the product's reaches the environment's
	// unit, runtime or record names.
	if strings.Contains(id, "codex") {
		t.Fatalf("instance %q names the plugin", id)
	}
	for name, script := range map[string]string{"session": linuxAppDesktop, "install": linuxAppInstall, "remove": linuxAppRemove} {
		if regexp.MustCompile(`desktop-app-\$app_id|desktop-\$id|desktop-\$app_id`).FindString(script) == "" && name != "session" {
			t.Fatalf("%s does not use the instance name", name)
		}
		cmd := exec.Command("bash", "-n")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", name, err, out)
		}
	}
}

func TestDesktopAppVariablesQuoteManifestValues(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "resolve.py"), []byte("print('{}')\n"), 0644); err != nil {
		t.Fatal(err)
	}
	target := desktopAppTarget{plugin: "p", dir: dir, id: desktopAppInstance("p", "a"), app: hostbridge.DesktopApp{
		ID: "a", Title: "A", Command: []string{"Tar App/run", "--flag=it's"}, Schemes: []string{"a-app"},
		Source: hostbridge.AppSource{Type: hostbridge.SourceArchive, Resolve: []string{"resolve.py", "--channel", "stable"}, Hosts: []string{"dl.example.com"}},
	}}
	variables, err := desktopAppVariables(target)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", variables+`printf '%s\n' "$app_id" "$app_command" "$app_resolver_kind" "$app_resolver_args" "$app_hosts" "$(printf '%s' "$app_resolver" | base64 -d)"`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	want := target.id + "\n" + `["Tar App/run","--flag=it's"]` + "\npy\n" + `["--channel","stable"]` + "\ndl.example.com\nprint('{}')\n"
	if string(out) != want {
		t.Fatalf("variables:\n%s\nwant:\n%s", out, want)
	}
	target.app.Source.Resolve = []string{"missing.py"}
	if _, err := desktopAppVariables(target); err == nil {
		t.Fatal("a missing resolver was accepted")
	}
}

// installerStubs are commands the installer meets before any privileged step
// a test must not take. sudo runs only reads and apt's own wrapper.
func installerStubs(t *testing.T) string {
	dir := t.TempDir()
	commands := map[string]string{
		"sudo":      "#!/bin/sh\ncase \"$1\" in env|test|find|grep) exec \"$@\";; esac\necho blocked-privileged-operation >&2; exit 97\n",
		"apt-get":   "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$HOME/apt-requests\"\n",
		"systemctl": "#!/bin/sh\nexit 3\n",
		"dpkg":      "#!/bin/sh\necho arm64\n",
		"gpg":       "#!/bin/sh\ncase \"$*\" in *--list-keys*) echo 'fpr:::::::::UNTRUSTEDKEY:';; esac\nexit 0\n",
	}
	for name, body := range commands {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func runInstaller(t *testing.T, target desktopAppTarget) (string, error) {
	// The installer runs inside the Linux environment and needs its tools
	// (flock, timeout, python3 on the standard path); hosts only check syntax.
	if runtime.GOOS != "linux" {
		t.Skip("the installer runs inside the Linux environment")
	}
	stubs := installerStubs(t)
	variables, err := desktopAppVariables(target)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(cage.ToolInstallPrelude() + "\ncage_curl_download() { : > \"$1\"; }\n" + variables + linuxAppInstall)
	cmd.Env = append(os.Environ(), "HOME="+stubs, "PATH="+stubs+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestDesktopAppInstallerRefusesUndeclaredDownloads(t *testing.T) {
	dir := t.TempDir()
	resolver := `import json; print(json.dumps({"version": "2.0.0", "url": "https://elsewhere.example.net/app.tar.gz"}))`
	if err := os.WriteFile(filepath.Join(dir, "resolve.py"), []byte(resolver), 0644); err != nil {
		t.Fatal(err)
	}
	target := desktopAppTarget{plugin: "p", dir: dir, id: desktopAppInstance("p", "a"), app: hostbridge.DesktopApp{
		ID: "a", Title: "A", Command: []string{"app"},
		Source: hostbridge.AppSource{Type: hostbridge.SourceArchive, Resolve: []string{"resolve.py"}, Hosts: []string{"dl.example.com"}},
	}}
	out, err := runInstaller(t, target)
	if err == nil || !strings.Contains(out, "下载地址不在插件声明的官方下载站点内") || strings.Contains(out, "blocked-privileged-operation") {
		t.Fatalf("undeclared host: %v\n%s", err, out)
	}
	for name, release := range map[string]string{
		"plain http":  `{"version": "2.0.0", "url": "http://dl.example.com/app.tar.gz"}`,
		"bad version": `{"version": "../2", "url": "https://dl.example.com/app.tar.gz"}`,
		"bad digest":  `{"version": "2.0.0", "url": "https://dl.example.com/app.tar.gz", "sha256": "abc"}`,
		"not json":    `version 2`,
	} {
		if err := os.WriteFile(filepath.Join(dir, "resolve.py"), []byte("print("+strconvQuote(release)+")"), 0644); err != nil {
			t.Fatal(err)
		}
		out, err := runInstaller(t, target)
		if err == nil || strings.Contains(out, "blocked-privileged-operation") {
			t.Fatalf("%s accepted: %v\n%s", name, err, out)
		}
	}
}

func TestDesktopAppInstallerRefusesAnUnexpectedKey(t *testing.T) {
	target := desktopAppTarget{plugin: "p", id: desktopAppInstance("p", "a"), app: hostbridge.DesktopApp{
		ID: "a", Title: "A", Command: []string{"/usr/bin/app"},
		Source: hostbridge.AppSource{Type: hostbridge.SourceApt, Repository: "https://apt.example.com/deb", Suite: "stable", Components: []string{"main"},
			Key: "https://apt.example.com/key.gpg", Fingerprint: "3BFA0E4AE8B8CC16A2D9BA684A3B4A566C4660E4", Package: "app"},
	}}
	out, err := runInstaller(t, target)
	if err == nil || !strings.Contains(out, "官方签名密钥与插件声明的指纹不符") || strings.Contains(out, "blocked-privileged-operation") {
		t.Fatalf("unexpected key: %v\n%s", err, out)
	}
}

func strconvQuote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", `\'`) + "'"
}

// A plugin application's display follows its viewer like a Desktop
// instance's, behind the same origin, size and environment checks.
func TestDesktopAppResizeHTTP(t *testing.T) {
	s := testServer(t)
	dir := filepath.Join(s.hostPluginDir(), "workshop")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"workshop","version":"1.0.0","description":"Runs a vendor application.","engines":{"cage":">=0.2.1"},"kind":"guest","permissions":[],
		"contributes":{"desktopApps":[{"id":"repo-app","title":"Repo App",
		"source":{"type":"apt","repository":"https://downloads.example.com/apt","suite":"stable","components":["main"],
		"key":"https://downloads.example.com/key.asc","fingerprint":"31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE","package":"repo-app"},
		"command":["/usr/bin/repo-app"]}]}}`
	if err := os.WriteFile(filepath.Join(dir, hostbridge.ManifestFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	path := "/api/desktop-apps/workshop/repo-app/resize"
	if w := request(s, "POST", path, "https://claude.ai", `{"width":1000,"height":800}`, true); w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin resize: %d", w.Code)
	}
	if w := request(s, "GET", path, s.origin, "", true); w.Code != http.StatusNotFound {
		t.Fatalf("resize by GET: %d", w.Code)
	}
	for _, body := range []string{
		`{"width":100,"height":800}`,
		`{"width":1000,"height":9000}`,
		`{"width":1000,"height":800,"scale":5}`,
		`{"width":1000,"height":800,"scale":"2;reboot"}`,
		`{"width":1000,"height":800,"display":":0"}`,
	} {
		if w := request(s, "POST", path, s.origin, body, true); w.Code != http.StatusBadRequest {
			t.Fatalf("invalid resize %s: %d", body, w.Code)
		}
	}
	// The route exists: a valid size reaches the environment check.
	if w := request(s, "POST", path, s.origin, `{"width":1000,"height":800}`, true); w.Code != http.StatusConflict {
		t.Fatalf("resize with a stopped environment: %d %s", w.Code, w.Body.String())
	}
	for _, other := range []string{"/api/desktop-apps/workshop/other-app/resize", "/api/desktop-apps/other/repo-app/resize"} {
		if w := request(s, "POST", other, s.origin, `{"width":1000,"height":800}`, true); w.Code != http.StatusNotFound {
			t.Fatalf("resize of %s: %d", other, w.Code)
		}
	}
	s.mu.Lock()
	s.preferences.HostPluginsOff = "workshop"
	s.mu.Unlock()
	if w := request(s, "POST", path, s.origin, `{"width":1000,"height":800}`, true); w.Code != http.StatusNotFound {
		t.Fatalf("resize of a disabled plugin's application: %d", w.Code)
	}
}

func TestDesktopAppSessionUsesHiDPIAndDynamicScaling(t *testing.T) {
	for _, want := range []string{
		`display_scale=2`,
		`printf '%s\n' "$display_scale" > "$XDG_RUNTIME_DIR/display-scale"`,
		`printf '%s\n' "$display_scale" > "$XDG_RUNTIME_DIR/base-display-scale"`,
		`geometry=$(awk -v scale="$display_scale" 'BEGIN {printf "%dx%d", 1280 * scale, 800 * scale}')`,
		`-geometry "$geometry"`,
		`/usr/local/bin/cage-display-governor`,
		`Xft/DPI %s\nGdk/WindowScalingFactor 1\nGdk/UnscaledDPI %s\n`,
		`printf '%s\n' "$!" > "$XDG_RUNTIME_DIR/xsettings.pid"`,
		`theme-variant`,
		`_NET_WM_STATE_MAXIMIZED_HORZ`,
		`_NET_WM_STATE_MAXIMIZED_VERT`,
		`WM_TRANSIENT_FOR`,
	} {
		if !strings.Contains(linuxAppDesktop, want) {
			t.Fatalf("linux-app-desktop.sh missing HiDPI/desktop alignment piece %q", want)
		}
	}
}

func TestDesktopAppDownloadResumesTransientErrorsAndStopsOn403(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the installer Bash script runs in the Linux guest, not the Windows host")
	}
	shell, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	start := strings.Index(linuxAppInstall, "download() {")
	end := strings.Index(linuxAppInstall[start:], "\npackage=")
	if start < 0 || end < 0 {
		t.Fatal("download function not found in linux-app-install.sh")
	}
	downloadFn := linuxAppInstall[start : start+end]

	home := t.TempDir()
	dest := filepath.Join(home, "app.archive")
	// Attempt 1 writes partial bytes and exits 28 (timeout); attempt 2 resumes
	// and prints "200" (matching tool-download.py) without leaking it to stdout.
	script := `set -euo pipefail
sleep() { :; }
app_id=0123456789abcdef01234567
release_version=2.35.2
release_url=https://storage.googleapis.com/antigravity-public/example.tar.gz
release_digest=
cage_curl_download() {
  local part="$1"
  local count=0
  [ -f "$HOME/attempt-count" ] && count=$(cat "$HOME/attempt-count")
  count=$((count + 1))
  printf '%s' "$count" > "$HOME/attempt-count"
  case "$*" in
    *"--continue-at -"*) ;;
    *) echo "missing --continue-at -" >&2; return 99 ;;
  esac
  if (( count == 1 )); then
    printf 'part1-' >> "$part"
    printf '000'
    return 28
  fi
  printf 'part2' >> "$part"
  printf '200'
  return 0
}
` + downloadFn + `
download "$DEST"
printf 'CONTENT=%s CALLS=%s\n' "$(cat "$DEST")" "$(cat "$HOME/attempt-count")"
`
	cmd := exec.Command(shell, "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(os.Environ(), "HOME="+home, "DEST="+dest)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("resumable download failed: %v\n%s", err, out)
	}
	text := string(out)
	if !strings.Contains(text, "CONTENT=part1-part2 CALLS=2") || strings.Contains(text, "200") {
		t.Fatalf("unexpected download output: %q", text)
	}

	// HTTP 403 (curl exit 22, http_code 403) must fail immediately without retrying.
	failScript := `set -euo pipefail
sleep() { :; }
app_id=0123456789abcdef01234567
release_version=2.35.2
release_url=https://storage.googleapis.com/antigravity-public/example.tar.gz
release_digest=
cage_curl_download() {
  echo call >> "$HOME/calls"
  printf '403'
  return 22
}
` + downloadFn + `
download "$DEST"
`
	cmd = exec.Command(shell, "-s")
	cmd.Stdin = strings.NewReader(failScript)
	cmd.Env = append(os.Environ(), "HOME="+home, "DEST="+dest)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("403 unexpectedly succeeded: %s", out)
	}
	calls, err := os.ReadFile(filepath.Join(home, "calls"))
	if err != nil || strings.Count(string(calls), "call") != 1 {
		t.Fatalf("403 retried unexpectedly: %q (%v)", string(calls), err)
	}
}

func TestDesktopAppSessionAllowedDuringJob(t *testing.T) {
	cases := []struct {
		name   string
		job    Job
		target string
		want   bool
	}{
		{"idle", Job{}, "antigravity/antigravity", true},
		{"other app install", Job{Running: true, Kind: "desktop-app-install", Target: "codex/codex"}, "antigravity/antigravity", true},
		{"other app update", Job{Running: true, Kind: "desktop-app-update", Target: "codex/codex"}, "antigravity/antigravity", true},
		{"other app remove", Job{Running: true, Kind: "desktop-app-remove", Target: "codex/codex"}, "antigravity/antigravity", true},
		{"same app install", Job{Running: true, Kind: "desktop-app-install", Target: "antigravity/antigravity"}, "antigravity/antigravity", false},
		{"untargeted app install", Job{Running: true, Kind: "desktop-app-install"}, "antigravity/antigravity", false},
		{"claude desktop install", Job{Running: true, Kind: "claude-desktop-install"}, "antigravity/antigravity", true},
		{"environment tools", Job{Running: true, Kind: "environment-tools"}, "antigravity/antigravity", true},
		{"vm stop", Job{Running: true, Kind: "stop"}, "antigravity/antigravity", false},
		{"settings", Job{Running: true, Kind: "settings"}, "antigravity/antigravity", false},
		{"unknown job", Job{Running: true, Kind: "future-vm-job"}, "antigravity/antigravity", false},
	}
	for _, tc := range cases {
		if got := desktopAppSessionAllowedDuringJob(tc.job, tc.target); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func installTestDesktopAppPlugin(t *testing.T, s *Server, plugin, appID, title string) {
	t.Helper()
	dir := filepath.Join(s.hostPluginDir(), plugin)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"` + plugin + `","version":"1.0.0","description":"Test app.","engines":{"cage":">=0.2.1"},"kind":"guest","permissions":[],
		"contributes":{"desktopApps":[{"id":"` + appID + `","title":"` + title + `",
		"source":{"type":"apt","repository":"https://downloads.example.com/apt","suite":"stable","components":["main"],
		"key":"https://downloads.example.com/key.asc","fingerprint":"31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE","package":"` + appID + `"},
		"command":["/usr/bin/` + appID + `"]}]}}`
	if err := os.WriteFile(filepath.Join(dir, hostbridge.ManifestFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDesktopAppStartStopConcurrentlyWithOtherAppInstall(t *testing.T) {
	s := testServer(t)
	installTestDesktopAppPlugin(t, s, "codex", "codex", "Codex")
	installTestDesktopAppPlugin(t, s, "antigravity", "antigravity", "Antigravity")
	if err := writePrivate(filepath.Join(s.instanceDir(), "cage-running.json"), map[string]int{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}

	// Simulate Codex installing in the global job slot.
	s.setJobForTest(Job{
		ID:      "codex-install-1",
		Kind:    "desktop-app-install",
		Target:  "codex/codex",
		Running: true,
		Message: "正在下载并安装 Codex，可在操作详情查看进度…",
	})

	started := make(chan string, 2)
	release := make(chan struct{})
	s.desktopAppOpRunner = func(ctx context.Context, target desktopAppTarget, action string) error {
		started <- target.app.ID + ":" + action
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Antigravity start succeeds concurrently while Codex is installing.
	w := request(s, "POST", "/api/desktop-apps/antigravity/antigravity/start", s.origin, "{}", true)
	if w.Code != http.StatusOK {
		t.Fatalf("antigravity start during codex install: %d %s", w.Code, w.Body.String())
	}
	var first desktopAppOpReply
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil || !first.Local || !first.Running || first.Target != "antigravity/antigravity" || first.Kind != "desktop-app-start" {
		t.Fatalf("unexpected start reply: %+v (%v)", first, err)
	}
	select {
	case got := <-started:
		if got != "antigravity:start" {
			t.Fatalf("unexpected started op: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("antigravity start did not run")
	}

	// Duplicate start joins idempotently; opposite stop conflicts while start is in flight.
	dup := request(s, "POST", "/api/desktop-apps/antigravity/antigravity/start", s.origin, "{}", true)
	if dup.Code != http.StatusOK {
		t.Fatalf("duplicate start: %d %s", dup.Code, dup.Body.String())
	}
	var second desktopAppOpReply
	if err := json.Unmarshal(dup.Body.Bytes(), &second); err != nil || second.ID != first.ID {
		t.Fatalf("duplicate start did not join: %+v vs %+v", second, first)
	}
	if opp := request(s, "POST", "/api/desktop-apps/antigravity/antigravity/stop", s.origin, "{}", true); opp.Code != http.StatusConflict {
		t.Fatalf("stop while start in flight: %d %s", opp.Code, opp.Body.String())
	}

	// Starting the app currently being installed (Codex) is rejected.
	if same := request(s, "POST", "/api/desktop-apps/codex/codex/start", s.origin, "{}", true); same.Code != http.StatusConflict {
		t.Fatalf("codex start while codex installing: %d %s", same.Code, same.Body.String())
	}

	// Starting a second global maintenance job (installing Antigravity) still conflicts.
	if secondInstall := request(s, "POST", "/api/desktop-apps/antigravity/antigravity/install", s.origin, "{}", true); secondInstall.Code != http.StatusConflict {
		t.Fatalf("antigravity install during codex install: %d %s", secondInstall.Code, secondInstall.Body.String())
	}

	// Snapshot reports busy state for Antigravity while running.
	antigravityID := desktopAppInstance("antigravity", "antigravity")
	if busy, action, msg, errText := s.desktopAppOps.snapshot(antigravityID); !busy || action != "start" || !strings.Contains(msg, "Antigravity") || errText != "" {
		t.Fatalf("unexpected busy snapshot: busy=%v action=%q msg=%q err=%q", busy, action, msg, errText)
	}

	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if busy, _, _, _ := s.desktopAppOps.snapshot(antigravityID); !busy {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if busy, _, _, _ := s.desktopAppOps.snapshot(antigravityID); busy {
		t.Fatal("antigravity start stayed busy after completion")
	}

	// Stop also runs concurrently while Codex is still installing, and surfaces failures in snapshot.
	s.desktopAppOpRunner = func(ctx context.Context, target desktopAppTarget, action string) error {
		return errors.New("请先安装 Antigravity")
	}
	if stopResp := request(s, "POST", "/api/desktop-apps/antigravity/antigravity/stop", s.origin, "{}", true); stopResp.Code != http.StatusOK {
		t.Fatalf("antigravity stop during codex install: %d %s", stopResp.Code, stopResp.Body.String())
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if busy, _, _, errText := s.desktopAppOps.snapshot(antigravityID); !busy && errText != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, _, _, errText := s.desktopAppOps.snapshot(antigravityID); !strings.Contains(errText, "请先安装 Antigravity") {
		t.Fatalf("expected failure in snapshot, got %q", errText)
	}

	// VM stop job blocks per-app session start.
	s.setJobForTest(Job{ID: "vm-stop-1", Kind: "stop", Running: true})
	if blocked := request(s, "POST", "/api/desktop-apps/antigravity/antigravity/start", s.origin, "{}", true); blocked.Code != http.StatusConflict {
		t.Fatalf("start during vm stop: %d %s", blocked.Code, blocked.Body.String())
	}

	// CancelAll interrupts an active per-app start cleanly without leaking an error.
	s.setJobForTest(Job{})
	cancelStarted := make(chan struct{})
	cancelObserved := make(chan struct{})
	s.desktopAppOpRunner = func(ctx context.Context, target desktopAppTarget, action string) error {
		close(cancelStarted)
		<-ctx.Done()
		close(cancelObserved)
		return ctx.Err()
	}
	if resp := request(s, "POST", "/api/desktop-apps/antigravity/antigravity/start", s.origin, "{}", true); resp.Code != http.StatusOK {
		t.Fatalf("start before cancelAll: %d %s", resp.Code, resp.Body.String())
	}
	select {
	case <-cancelStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("start before cancelAll did not begin")
	}
	s.desktopAppOps.cancelAll()
	select {
	case <-cancelObserved:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelAll did not cancel active start")
	}
}

func TestDesktopAppMaintenanceJobCarriesTarget(t *testing.T) {
	s := testServer(t)
	installTestDesktopAppPlugin(t, s, "codex", "codex", "Codex")
	if err := writePrivate(filepath.Join(s.instanceDir(), "cage-running.json"), map[string]int{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	w := request(s, "POST", "/api/desktop-apps/codex/codex/install", s.origin, "{}", true)
	if w.Code != http.StatusOK {
		t.Fatalf("codex install: %d %s", w.Code, w.Body.String())
	}
	var job Job
	if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if job.Kind != "desktop-app-install" || job.Target != "codex/codex" {
		t.Fatalf("expected target codex/codex on install job, got %+v", job)
	}
	s.jobs.cancelRunning()
	s.jobs.close()
}
