package workbench

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

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
	cmd.Stdin = strings.NewReader("cage_curl_download() { : > \"$1\"; }\n" + variables + linuxAppInstall)
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
