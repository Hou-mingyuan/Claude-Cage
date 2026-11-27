package cage

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Programs that read a login shell's environment must receive only standard
// settings from the guest profile, whatever the service environment holds.
func TestGuestShellProfileExportsOnlyStandardSettings(t *testing.T) {
	_, profile, ok := strings.Cut(guestServices, "sudo tee /etc/profile.d/claude-cage.sh >/dev/null <<'SCRIPT'\n")
	if !ok {
		t.Fatal("missing shell profile")
	}
	profile, _, _ = strings.Cut(profile, "\nSCRIPT\n")
	dir := t.TempDir()
	environment := filepath.Join(dir, "environment")
	// The file as an existing guest still has it, without a final newline.
	if err := os.WriteFile(environment, []byte("TZ=Asia/Shanghai\nLANG=zh_CN.UTF-8\nLC_ALL=zh_CN.UTF-8\nCAGE_LANGUAGE=zh-CN\nBROWSER=/usr/local/bin/cage-open\nPULSE_SERVER=unix:/run/cage-audio/native"), 0600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "profile.sh")
	if err := os.WriteFile(file, []byte(strings.ReplaceAll(profile, "/etc/claude-cage/environment", environment)), 0600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		env     []string
		browser string
		pulse   string
	}{
		{"service values", []string{"CAGE_LANGUAGE=en-US", "BROWSER=/usr/local/bin/cage-open", "PULSE_SERVER=unix:/run/cage-audio/native"}, "/usr/local/bin/xdg-open", ""},
		{"empty", nil, "/usr/local/bin/xdg-open", ""},
		{"launcher values", []string{"BROWSER=/tmp/runtime-1/opener.py", "PULSE_SERVER=unix:/tmp/runtime-1/paths/audio"}, "/tmp/runtime-1/opener.py", "unix:/tmp/runtime-1/paths/audio"},
	}
	for _, shell := range []string{"sh", "bash", "zsh"} {
		if _, err := exec.LookPath(shell); err != nil {
			t.Logf("%s unavailable", shell)
			continue
		}
		for _, c := range cases {
			t.Run(shell+"/"+c.name, func(t *testing.T) {
				// Inherited values are exported by the command itself: a zsh
				// reads the system zshenv before the command runs.
				inherited := "export https_proxy=http://127.0.0.1:1"
				for _, value := range c.env {
					inherited += " '" + value + "'"
				}
				cmd := exec.Command(shell, "-c", inherited+`; . "$1"; env`, shell, file)
				cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir}
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("%v: %s", err, out)
				}
				got := map[string]string{}
				for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
					key, value, _ := strings.Cut(line, "=")
					got[key] = value
				}
				want := map[string]string{"TZ": "Asia/Shanghai", "LANG": "zh_CN.UTF-8", "LC_ALL": "zh_CN.UTF-8", "BROWSER": c.browser, "PULSE_SERVER": c.pulse}
				for key, value := range want {
					if got[key] != value {
						t.Errorf("%s=%q, want %q", key, got[key], value)
					}
				}
				// The account name and working directories are not set by the profile.
				var names []string
				for key, value := range got {
					if key == "HOME" || key == "PWD" || key == "OLDPWD" || key == "LOGNAME" || key == "USER" {
						continue
					}
					if strings.HasPrefix(strings.ToUpper(key), "CAGE") || strings.HasPrefix(key, "env_") || strings.Contains(strings.ToLower(value), "cage") || strings.Contains(strings.ToLower(key), "proxy") {
						names = append(names, key)
					}
				}
				sort.Strings(names)
				if len(names) > 0 {
					t.Errorf("shell environment keeps %v: %s", names, out)
				}
			})
		}
	}
}

func TestGuestEnvConfiguresCrossFilesystemGitDefaults(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	_, snippet, ok := strings.Cut(guestEnv, "if command -v git >/dev/null 2>&1; then\n")
	if !ok {
		t.Fatal("missing git performance defaults in guest-env.sh")
	}
	snippet = "if command -v git >/dev/null 2>&1; then\n" + snippet
	dir := t.TempDir()
	sysCfg := filepath.Join(dir, "gitconfig")
	script := strings.ReplaceAll(snippet, "sudo git ", "git ")
	run := func() {
		cmd := exec.Command("bash", "-euo", "pipefail", "-c", script)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_SYSTEM="+sysCfg, "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git defaults snippet failed: %v\n%s", err, out)
		}
	}
	run()
	get := func(key string) string {
		cmd := exec.Command("git", "config", "--file", sysCfg, "--get", key)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("read %s: %v (%s)", key, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if got := get("core.checkStat"); got != "minimal" {
		t.Fatalf("core.checkStat=%q, want minimal", got)
	}
	if got := get("core.trustctime"); got != "false" {
		t.Fatalf("core.trustctime=%q, want false", got)
	}
	if got := get("core.preloadIndex"); got != "true" {
		t.Fatalf("core.preloadIndex=%q, want true", got)
	}
	data, err := os.ReadFile(sysCfg)
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(data))
	for _, forbidden := range []string{"cage", "guest", "virtual machine", "vm"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("system gitconfig leaked %q: %s", forbidden, data)
		}
	}
	// Existing explicit overrides must be preserved on subsequent starts.
	if out, err := exec.Command("git", "config", "--file", sysCfg, "core.checkStat", "default").CombinedOutput(); err != nil {
		t.Fatalf("override core.checkStat: %v (%s)", err, out)
	}
	run()
	if got := get("core.checkStat"); got != "default" {
		t.Fatalf("existing core.checkStat was overwritten: %q", got)
	}
}
