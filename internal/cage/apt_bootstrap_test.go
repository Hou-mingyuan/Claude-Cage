package cage

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAPTBootstrapUpgradeFailureAndRetry(t *testing.T) {
	for _, mode := range []string{"current", "upgrade", "download-failure", "stale-candidate", "unconfigured", "no-utils"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			version := "3.0.3"
			if mode == "current" {
				version = "3.0.4"
			}
			os.WriteFile(filepath.Join(dir, "version"), []byte(version), 0600)
			os.WriteFile(filepath.Join(dir, "os-release"), []byte("ID=debian\nVERSION_ID=13\n"), 0600)
			os.WriteFile(filepath.Join(dir, "sentinel"), []byte("personal data stays"), 0600)
			commands := map[string]string{
				"sudo": "#!/bin/sh\nexec \"$@\"\n",
				"dpkg-query": `#!/bin/sh
case "$*" in
 *apt-utils*)
  case "$MODE" in no-utils) exit 1;; unconfigured) echo 'install ok unpacked';; *) echo 'install ok installed';; esac;;
 *) cat "$FIXTURE/version";;
esac
`,
				"dpkg": `#!/bin/sh
[ "$2" != 3.0.3 ]
`,
				"apt-get": `#!/bin/sh
printf '%s\n' "$*" >> "$FIXTURE/requests"
case "$*" in
 *Acquire::Retries=0*) ;; *) exit 92;;
esac
case "$*" in
 *'install '*)
  [ "$MODE" != download-failure ] || exit 100
  [ "$MODE" = stale-candidate ] || echo 3.0.4 > "$FIXTURE/version";;
esac
`,
			}
			for name, body := range commands {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0755); err != nil {
					t.Fatal(err)
				}
			}
			script := strings.ReplaceAll(APTBootstrapScript, ". /etc/os-release", ". \"$FIXTURE/os-release\"")
			run := func() ([]byte, error) {
				cmd := exec.Command("sh", "-c", script+"\nprintf continued >> \"$FIXTURE/continued\"\n")
				cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "FIXTURE="+dir, "MODE="+mode, "TMPDIR="+dir)
				return cmd.CombinedOutput()
			}
			out, err := run()
			requests, _ := os.ReadFile(filepath.Join(dir, "requests"))
			fail := mode == "download-failure" || mode == "stale-candidate"
			if fail {
				if err == nil {
					t.Fatalf("failure continued: %s", out)
				}
				if mode == "download-failure" && !strings.Contains(string(out), "APT_FAILED exit=100 command=apt-get ") {
					t.Fatalf("failed bootstrap did not report its APT command: %s", out)
				}
				if _, err := os.Stat(filepath.Join(dir, "continued")); !os.IsNotExist(err) {
					t.Fatal("continued after failed bootstrap")
				}
				mode = "upgrade"
				if out, err := run(); err != nil {
					t.Fatalf("retry failed: %v %s", err, out)
				}
			} else if err != nil {
				t.Fatalf("%v %s", err, out)
			}
			if mode == "current" && len(requests) != 0 {
				t.Fatal("current version used network")
			}
			if strings.Contains(string(requests), "apt-utils") && (mode == "unconfigured" || mode == "no-utils") {
				t.Fatal("upgraded uninstalled apt-utils")
			}
			if data, _ := os.ReadFile(filepath.Join(dir, "sentinel")); string(data) != "personal data stays" {
				t.Fatal("personal data changed")
			}
		})
	}
}

func TestAPTBootstrapPrefersHTTPSAndFallsBackToHTTP(t *testing.T) {
	for _, tc := range []struct {
		mode, want string
		ok         bool
	}{
		{"https-only-network", "https\n", true},
		{"http-only-network", "https\nhttp\n", true},
		{"no-ca-bundle", "http\n", true},
		{"offline", "https\nhttp\n", false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "version"), []byte("3.0.3"), 0600)
			os.WriteFile(filepath.Join(dir, "os-release"), []byte("ID=debian\nVERSION_ID=13\n"), 0600)
			if tc.mode != "no-ca-bundle" {
				os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("bundle"), 0600)
			}
			commands := map[string]string{
				"sudo":       "#!/bin/sh\nexec \"$@\"\n",
				"dpkg-query": "#!/bin/sh\ncat \"$FIXTURE/version\"\n",
				"dpkg":       "#!/bin/sh\n[ \"$2\" != 3.0.3 ]\n",
				"apt-get": `#!/bin/sh
for arg; do
  case "$arg" in Dir::Etc::sourcelist=*) list=${arg#Dir::Etc::sourcelist=};; esac
done
case "$(cat "$list")" in
 *' https://deb.debian.org/'*) scheme=https;;
 *' http://deb.debian.org/'*) scheme=http;;
 *) exit 91;;
esac
case "$*" in
 *' update')
  printf '%s\n' "$scheme" >> "$FIXTURE/updates"
  case "$MODE:$scheme" in
   offline:*|https-only-network:http|http-only-network:https) echo "Connection failed" >&2; exit 100;;
  esac;;
 *'install '*) echo 3.0.4 > "$FIXTURE/version";;
esac
`,
			}
			for name, body := range commands {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0755); err != nil {
					t.Fatal(err)
				}
			}
			script := strings.ReplaceAll(APTBootstrapScript, ". /etc/os-release", ". \"$FIXTURE/os-release\"")
			script = strings.ReplaceAll(script, "/etc/ssl/certs/ca-certificates.crt", "$FIXTURE/ca.crt")
			cmd := exec.Command("sh", "-c", script+"\nprintf continued >> \"$FIXTURE/continued\"\n")
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "FIXTURE="+dir, "MODE="+tc.mode, "TMPDIR="+dir)
			out, err := cmd.CombinedOutput()
			updates, _ := os.ReadFile(filepath.Join(dir, "updates"))
			_, continued := os.Stat(filepath.Join(dir, "continued"))
			if string(updates) != tc.want || (err == nil) != tc.ok || (continued == nil) != tc.ok {
				t.Fatalf("sources tried %q, err %v: %s", updates, err, out)
			}
			if tc.ok && strings.Contains(string(out), "APT_FAILED") {
				t.Fatalf("recovered fallback emitted stale APT_FAILED marker: %s", out)
			}
			if !tc.ok && !strings.Contains(string(out), "APT_FAILED exit=100 command=apt-get update") {
				t.Fatalf("failed bootstrap did not report its APT command: %s", out)
			}
		})
	}
}

func TestAPTBootstrapFinishesInterruptedPackageInstallation(t *testing.T) {
	for _, mode := range []string{"interrupted", "clean", "repair-fails"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "os-release"), []byte("ID=debian\nVERSION_ID=13\n"), 0600)
			os.MkdirAll(filepath.Join(dir, "updates"), 0700)
			if mode != "clean" {
				os.WriteFile(filepath.Join(dir, "updates", "0001"), nil, 0600)
			}
			commands := map[string]string{
				"sudo":       "#!/bin/sh\nexec \"$@\"\n",
				"dpkg-query": "#!/bin/sh\necho 3.0.4\n",
				"dpkg": `#!/bin/sh
case "$*" in
 '--configure -a')
  printf 'configure\n' >> "$FIXTURE/requests"
  [ "$MODE" != repair-fails ] || exit 1
  rm -f "$FIXTURE"/updates/*;;
esac
`,
				"apt-get": "#!/bin/sh\nprintf 'apt-get %s\\n' \"$*\" >> \"$FIXTURE/requests\"\n",
			}
			for name, body := range commands {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0755); err != nil {
					t.Fatal(err)
				}
			}
			script := strings.ReplaceAll(APTBootstrapScript, ". /etc/os-release", ". \"$FIXTURE/os-release\"")
			script = strings.ReplaceAll(script, "/var/lib/dpkg/updates", "$FIXTURE/updates")
			cmd := exec.Command("sh", "-c", script+"\nprintf continued >> \"$FIXTURE/requests\"\n")
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "FIXTURE="+dir, "MODE="+mode, "TMPDIR="+dir)
			out, err := cmd.CombinedOutput()
			requests, _ := os.ReadFile(filepath.Join(dir, "requests"))
			want := map[string]string{"interrupted": "configure\ncontinued", "clean": "continued", "repair-fails": "configure\n"}[mode]
			if string(requests) != want || (err != nil) != (mode == "repair-fails") {
				t.Fatalf("requests %q, err %v: %s", requests, err, out)
			}
		})
	}
}
func TestGuestInstallEntrypointsCheckAPTFirst(t *testing.T) {
	for name, script := range map[string]string{"lima": DebianPrepareScript, "services": GuestSetup, "start": DefaultConfig().StartScript(), "tools": ToolsUpdateScript()} {
		gate := strings.Index(script, "CAGE_APT_BOOTSTRAP")
		install := strings.Index(script, "apt-get install")
		if gate < 0 || (install >= 0 && gate > install) {
			t.Fatalf("%s installs before APT check", name)
		}
	}
}
