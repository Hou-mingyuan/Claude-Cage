package cage

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGuestManifestIsPublishedOnlyAfterSuccessfulServiceStart(t *testing.T) {
	for _, initial := range []string{"missing", "old", "current"} {
		for _, failure := range []string{"none", "body", "publish"} {
			t.Run(initial+"/"+failure, func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "components.json")
				pending := filepath.Join(dir, "components.updating")
				previous := RequiredGuestManifest()
				if initial == "old" {
					previous.Revision = "previous"
				}
				old, _ := json.Marshal(previous)
				if initial != "missing" {
					if err := os.WriteFile(path, old, 0644); err != nil {
						t.Fatal(err)
					}
				}
				body := `test -f "$MANIFEST_DIR/components.updating"
if [ "$INITIAL" != missing ]; then test -f "$MANIFEST_DIR/components.json"; fi
printf applied > "$MANIFEST_DIR/applied"
`
				startBody := body
				if failure == "body" {
					startBody += "exit 7\n"
				}
				out, err := runGuestManifestStart(t, dir, startBody, initial, failure == "publish")
				if (err != nil) != (failure != "none") {
					t.Fatalf("start: %v\n%s", err, out)
				}
				data, readErr := os.ReadFile(path)
				if failure == "none" {
					var manifest GuestManifest
					if readErr != nil || json.Unmarshal(data, &manifest) != nil || !manifest.Current() {
						t.Fatalf("manifest not published: %s, %v", data, readErr)
					}
					if _, err := os.Stat(pending); !os.IsNotExist(err) {
						t.Fatal("successful start retained the pending marker", err)
					}
				} else {
					if initial == "missing" {
						if !os.IsNotExist(readErr) {
							t.Fatal("failed start published a manifest", readErr)
						}
					} else if readErr != nil || string(data) != string(old) {
						t.Fatalf("failed start damaged the previous manifest: %s, %v", data, readErr)
					}
					if _, err := os.Stat(pending); err != nil {
						t.Fatal("failed start lost the recovery marker", err)
					}
					// Replaying the start body recovers without downloading anything.
					if out, err := runGuestManifestStart(t, dir, body, initial, false); err != nil {
						t.Fatalf("recovery failed: %v\n%s", err, out)
					}
					var manifest GuestManifest
					data, err := os.ReadFile(path)
					if err != nil || json.Unmarshal(data, &manifest) != nil || !manifest.Current() {
						t.Fatalf("recovery did not publish manifest: %s, %v", data, err)
					}
				}
				if entries, _ := filepath.Glob(filepath.Join(dir, ".components-*")); len(entries) != 0 {
					t.Fatal("temporary manifests left behind", entries)
				}
			})
		}
	}
}

func runGuestManifestStart(t *testing.T, dir, body, initial string, failPublish bool) ([]byte, error) {
	t.Helper()
	python := "python3"
	if runtime.GOOS == "windows" {
		python = "python"
	}
	python, err := exec.LookPath(python)
	if err != nil {
		t.Fatal("Python 3 is required", err)
	}
	// Execute the generated shell and Python against real files. Instrument the
	// replace boundary to check that readers still see the previous complete file.
	bootstrap := `import os,sys,json
from pathlib import Path
root=Path(os.environ['MANIFEST_DIR'])
original_replace=os.replace
def replace(src,dst):
 assert Path(src).parent==Path(dst).parent==root
 assert (root/'components.updating').exists()
 assert (root/'applied').exists()
 json.loads(Path(src).read_text())
 if os.environ['INITIAL']!='missing': json.loads(Path(dst).read_text())
 if os.environ['FAIL_PUBLISH']=='true': raise OSError('injected replace failure')
 original_replace(src,dst)
os.replace=replace
# Windows cannot fsync directories. Keep real file fsync/replace operations,
# and emulate only the guest's directory handles on this test host.
if os.name=='nt':
 original_open,original_fsync,original_close=os.open,os.fsync,os.close
 os.open=lambda path,flags,*args,**kwargs: -99 if Path(path)==root else original_open(path,flags,*args,**kwargs)
 os.fsync=lambda fd: None if fd==-99 else original_fsync(fd)
 os.close=lambda fd: None if fd==-99 else original_close(fd)
exec(sys.stdin.read())
`
	prelude := `sudo() {
 case "$1" in
  python3) "$TEST_PYTHON" -c "$TEST_BOOTSTRAP";;
  rm) rm -f "$MANIFEST_DIR/components.json";;
  *) return 91;;
 esac
}
`
	cmd := exec.Command("bash", "-c", prelude+strings.ReplaceAll(markGuestStart(body), "/etc/claude-cage", filepath.ToSlash(dir)))
	cmd.Env = append(os.Environ(), "TEST_PYTHON="+filepath.ToSlash(python), "TEST_BOOTSTRAP="+bootstrap, "MANIFEST_DIR="+filepath.ToSlash(dir), "INITIAL="+initial, fmt.Sprintf("FAIL_PUBLISH=%t", failPublish))
	return cmd.CombinedOutput()
}

func TestToolDownloadWithRealCurlAndTLS(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl unavailable")
	}
	dir := t.TempDir()
	if _, err := exec.LookPath("sha256sum"); err != nil || runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("shasum"); err != nil {
			t.Skip("SHA-256 tool unavailable")
		}
		if err := os.WriteFile(filepath.Join(dir, "sha256sum"), []byte("#!/bin/sh\nexec shasum -a 256 \"$@\"\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	data := "complete download bytes"
	var calls atomic.Int32
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			return
		}
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			fmt.Fprint(w, data[:5])
			return
		}
		if r.Header.Get("Range") != "bytes=5-" {
			t.Errorf("range: %q", r.Header.Get("Range"))
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 5-%d/%d", len(data)-1, len(data)))
		w.WriteHeader(206)
		fmt.Fprint(w, data[5:])
	}))
	defer s.Close()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(data))
	cmd := exec.Command("bash", "-c", "set -euo pipefail\n"+ToolInstallPrelude()+`cage_download_tool "$DOWNLOAD_URL" "$DOWNLOAD_HASH" "$HOME/package"
cage_download_tool "$DOWNLOAD_URL" "$DOWNLOAD_HASH" "$HOME/package"`)
	cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+dir+":"+os.Getenv("PATH"), "CURL_CA_BUNDLE="+ca, "DOWNLOAD_URL="+s.URL, fmt.Sprintf("DOWNLOAD_HASH=%x", hash), "NO_PROXY=127.0.0.1", "no_proxy=127.0.0.1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(dir, "package"))
	if err != nil || string(got) != data || calls.Load() != 2 {
		t.Fatalf("%q %v calls=%d", got, err, calls.Load())
	}
}

func TestToolDownloadIdleTimeoutAndManualResume(t *testing.T) {
	for _, tool := range []string{"curl", "python3", "bash", "shasum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable")
		}
	}
	for _, mode := range []string{"slow", "stalled", "no-body"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range map[string]string{
				"sha256sum": "#!/bin/sh\nexec shasum -a 256 \"$@\"\n",
				"sleep":     "#!/bin/sh\nexit 0\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0755); err != nil {
					t.Fatal(err)
				}
			}
			const data = "abcdefghijklmnop"
			var calls atomic.Int32
			var recovered atomic.Bool
			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					w.Header().Set("Content-Length", fmt.Sprint(len(data)))
					return
				}
				call := calls.Add(1)
				offset := 0
				if mode == "stalled" {
					offset = int(call - 1)
				}
				wantRange := ""
				if offset > 0 {
					wantRange = fmt.Sprintf("bytes=%d-", offset)
				}
				if r.Header.Get("Range") != wantRange {
					t.Errorf("range = %q, want %q", r.Header.Get("Range"), wantRange)
				}
				w.Header().Set("Content-Length", fmt.Sprint(len(data)-offset))
				if offset > 0 {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(data)-1, len(data)))
					w.WriteHeader(http.StatusPartialContent)
				}
				if recovered.Load() {
					fmt.Fprint(w, data[offset:])
					return
				}
				if mode == "slow" {
					for i := range len(data) {
						fmt.Fprint(w, data[i:i+1])
						w.(http.Flusher).Flush()
						select {
						case <-r.Context().Done():
							return
						case <-time.After(100 * time.Millisecond):
						}
					}
					return
				}
				if mode == "stalled" {
					fmt.Fprint(w, data[offset:offset+1])
				}
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer s.Close()
			ca := filepath.Join(dir, "ca.pem")
			if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0600); err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256([]byte(data))
			run := func() ([]byte, error) {
				// Shorten only the test's idle interval; use the real curl, TLS,
				// filesystem, checksum, automatic retries and manual continuation.
				// The idle clock starts before curl connects, so leave room for a
				// loaded CI host to start curl and finish the TLS handshake.
				prelude := strings.Replace(ToolInstallPrelude(), "idle_timeout = 60", "idle_timeout = 2", 1)
				cmd := exec.CommandContext(t.Context(), "bash", "-c", prelude+`cage_download_tool "$DOWNLOAD_URL" "$DOWNLOAD_HASH" "$HOME/package"`)
				cmd.Env = append(os.Environ(), "CAGE_SETUP_PROGRESS=1", "HOME="+dir, "PATH="+dir+":"+os.Getenv("PATH"), "CURL_CA_BUNDLE="+ca, "DOWNLOAD_URL="+s.URL, fmt.Sprintf("DOWNLOAD_HASH=%x", hash), "NO_PROXY=127.0.0.1", "no_proxy=127.0.0.1")
				return cmd.CombinedOutput()
			}
			out, err := run()
			if mode == "slow" {
				if !strings.Contains(string(out), "CAGE_TOOL_BYTES package 0") || !strings.Contains(string(out), "CAGE_TOOL_BYTES package 16") {
					t.Fatalf("real tool transfer did not publish its byte counts: %s", out)
				}
				if err != nil || calls.Load() != 1 {
					t.Fatalf("progressing download interrupted: %v calls=%d %s", err, calls.Load(), out)
				}
			} else {
				if err == nil || calls.Load() != 4 || !strings.Contains(string(out), "no data") {
					t.Fatalf("stalled download did not exhaust idle retries: %v calls=%d %s", err, calls.Load(), out)
				}
				if _, err := os.Stat(filepath.Join(dir, "package")); !os.IsNotExist(err) {
					t.Fatal("incomplete file published")
				}
				if mode == "stalled" {
					got, err := os.ReadFile(filepath.Join(dir, "package.part"))
					if err != nil || string(got) != data[:4] {
						t.Fatalf("partial data lost: %q %v", got, err)
					}
				}
				recovered.Store(true)
				if out, err := run(); err != nil || calls.Load() != 5 {
					t.Fatalf("manual continuation failed: %v calls=%d %s", err, calls.Load(), out)
				}
			}
			got, err := os.ReadFile(filepath.Join(dir, "package"))
			if err != nil || string(got) != data {
				t.Fatalf("download not verified and published: %q %v", got, err)
			}
		})
	}
}

func TestClaudeNativeCacheUsesGuestArchitectureAndIntegrity(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq unavailable")
	}
	for _, arch := range []string{"arm64", "amd64"} {
		t.Run(arch, func(t *testing.T) {
			dir := t.TempDir()
			hash := sha512.Sum512([]byte("native-package"))
			packageName := "claude-code-linux-arm64"
			if arch == "amd64" {
				packageName = "claude-code-linux-x64"
			}
			commands := map[string]string{
				"dpkg":   "#!/bin/sh\nif [ \"$1\" = --print-architecture ]; then echo \"$TEST_ARCH\"; else exit 1; fi\n",
				"claude": "#!/bin/sh\necho 1.0.0\n",
				"flock":  "#!/bin/sh\nexit 0\n",
				"curl": `#!/bin/sh
for arg in "$@"; do [ "$arg" != --head ] || exit 0; done
while [ "$#" -gt 0 ]; do
  case "$1" in
    https://cdn.npmmirror.com/packages/%40anthropic-ai/"$TEST_PACKAGE"/9.8.7/"$TEST_PACKAGE"-9.8.7.tgz)
      body=native-package
      [ "$TEST_ARCH" != amd64 ] || body=corrupt-mirror
      echo mirror >> "$HOME/sources";;
    https://registry.npmjs.org/"$TEST_PACKAGE".tgz)
      body=native-package
      echo upstream >> "$HOME/sources";;
    -o) shift; printf %s "${body:?unexpected source}" > "$1"; printf 200; exit;;
  esac
  shift
done
exit 1
`,
				"npm": `#!/bin/sh
case "$1" in
view)
  if [ "$3" = @anthropic-ai/claude-code@latest ]; then
    test "$4 $5" = 'version --json' || exit 96
    printf '"9.8.7"'; exit
  fi
  test "$3" = "@anthropic-ai/$TEST_PACKAGE@9.8.7" || exit 91
  printf '{"integrity":"sha512-%s","tarball":"https://registry.npmjs.org/%s.tgz"}' "$TEST_INTEGRITY" "$TEST_PACKAGE";;
cache)
  test "$2 $3 $4" = 'add --offline --ignore-scripts' || exit 92
  test "$(cat "$5")" = native-package || exit 93
  printf cached > "$HOME/cached";;
install)
  test -f "$HOME/cached" || exit 94
  test "$3" = --prefix && test "$4" = "$HOME/.local" && test "$5" = --global || exit 97
  test "$6" = '@anthropic-ai/claude-code@9.8.7' || exit 97
  printf installed > "$HOME/installed";;
prefix)
  test "$2" = --global || exit 99
  printf '/usr\n';;
config)
  test -f "$HOME/installed" || exit 90
  test "$2 $3" = 'set --location=user' && test "$4" = "prefix=$HOME/.local" || exit 99
  printf '%s\n' "$4" > "$HOME/npm-prefix";;
*) exit 95;;
esac
`,
			}
			if _, err := exec.LookPath("sha512sum"); err != nil || runtime.GOOS == "darwin" {
				if _, err := exec.LookPath("shasum"); err != nil {
					t.Skip("SHA-512 tool unavailable")
				}
				commands["sha512sum"] = "#!/bin/sh\nexec shasum -a 512 \"$@\"\n"
			}
			for name, body := range commands {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0755); err != nil {
					t.Fatal(err)
				}
			}
			// The simulated amd64 guest may be running on an arm64 test host.
			cpuinfo := filepath.Join(dir, "cpuinfo")
			if err := os.WriteFile(cpuinfo, []byte("flags\t: ssse3 sse4_1 sse4_2 popcnt\n"), 0600); err != nil {
				t.Fatal(err)
			}
			prelude := strings.ReplaceAll(ToolInstallPrelude(), "/proc/cpuinfo", `"$HOME/cpuinfo"`)
			cmd := exec.Command("bash", "-c", prelude+"cage_install_tool claude")
			cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+dir+":"+os.Getenv("PATH"), "TEST_ARCH="+arch, "TEST_PACKAGE="+packageName, "TEST_INTEGRITY="+base64.StdEncoding.EncodeToString(hash[:]))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v %s", err, out)
			}
			if _, err := os.Stat(filepath.Join(dir, "installed")); err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(filepath.Join(dir, "npm-prefix")); err != nil || string(got) != "prefix="+filepath.Join(dir, ".local")+"\n" {
				t.Fatalf("fresh install did not configure npm prefix: %q %v", got, err)
			}
			want := "upstream\n"
			if got, err := os.ReadFile(filepath.Join(dir, "sources")); err != nil || string(got) != want {
				t.Fatalf("sources: %q, want %q (%v)", got, want, err)
			}
			if arch == "amd64" {
				if err := os.WriteFile(cpuinfo, []byte("flags\t: sse2 sse4a popcnt\n"), 0600); err != nil {
					t.Fatal(err)
				}
				retry := exec.Command("bash", "-c", prelude+"cage_install_tool claude")
				retry.Env = cmd.Env
				if out, err := retry.CombinedOutput(); err == nil || !strings.Contains(string(out), "需要 SSE4.2") {
					t.Fatalf("missing guest CPU feature should stop Claude setup: %v %s", err, out)
				}
			}
		})
	}
}

func TestClaudeLatestResolutionPreservesNewerInstallAndRejectsBadMetadata(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq unavailable")
	}
	for _, metadata := range []string{`"9.8.7"`, `"latest"`, `{}`, `"9.8.7;touch bad"`, `null`} {
		t.Run(metadata, func(t *testing.T) {
			dir := t.TempDir()
			// Exercise the real installation entry point. Resolving latest is
			// mandatory even when an existing install satisfies the old floor.
			cmd := exec.Command("bash", "-c", ToolInstallPrelude()+`
dpkg() {
  if [ "$1" = --print-architecture ]; then echo arm64; return; fi
  test "$*" = '--compare-versions 9.9.9 ge 9.8.7'
}
claude() { echo '9.9.9 (Claude Code)'; }
npm() {
  printf '%s\n' "$*" >> "$HOME/npm-calls"
  test "$*" = 'view --registry=https://registry.npmjs.org @anthropic-ai/claude-code@latest version --json' || return 98
  printf '%s' "$TEST_METADATA"
}
cage_install_tool claude`)
			cmd.Env = append(os.Environ(), "HOME="+dir, "TEST_METADATA="+metadata)
			out, err := cmd.CombinedOutput()
			calls, readErr := os.ReadFile(filepath.Join(dir, "npm-calls"))
			if readErr != nil || string(calls) != "view --registry=https://registry.npmjs.org @anthropic-ai/claude-code@latest version --json\n" {
				t.Fatalf("resolution reached cache/install or repeated the lookup: %q %v", calls, readErr)
			}
			if metadata == `"9.8.7"` {
				if err != nil || !strings.Contains(string(out), "already meets required version 9.8.7") {
					t.Fatalf("newer install changed or latest not resolved: %v %s", err, out)
				}
			} else if err == nil {
				t.Fatalf("bad metadata accepted: %s", out)
			}
		})
	}
}

func TestClaudeToolsUpdateRepairsExistingNPMPrefixWithoutReinstalling(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq unavailable")
	}
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm unavailable")
	}
	for _, install := range []string{"npm", "native"} {
		t.Run(install, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command("bash", "-c", ToolInstallPrelude()+`
set -euo pipefail
unset npm_config_prefix NPM_CONFIG_PREFIX npm_config_userconfig NPM_CONFIG_USERCONFIG
export npm_config_userconfig="$HOME/.npmrc" npm_config_globalconfig="$HOME/global-npmrc"
export PATH="$HOME/.local/bin:$PATH"
package="$HOME/.local/lib/node_modules/@anthropic-ai/claude-code/bin"
mkdir -p "$HOME/.local/bin" "$package"
printf 'fund=false\n' > "$HOME/.npmrc"
printf 'prefix=/usr\nfetch-retries=1\n' > "$HOME/global-npmrc"
printf '#!/bin/sh\nprintf "9.9.9 (Claude Code)\\n"\n' > "$package/claude.exe"
chmod +x "$package/claude.exe"
if [ "$TEST_INSTALL" = npm ]; then
  ln -s ../lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe "$HOME/.local/bin/claude"
else
  # An old npm package may still be on disk after a native install.
  cp "$package/claude.exe" "$HOME/.local/bin/claude"
fi
dpkg() {
  if [ "$1" = --print-architecture ]; then echo arm64; return; fi
  test "$*" = '--compare-versions 9.9.9 ge 9.8.7'
}
npm() {
  if [ "$1" = view ]; then printf '"9.8.7"'; return; fi
  if [ "$1" = config ]; then printf 'config\n' >> "$HOME/config-writes"; fi
  case "$1" in prefix|config) command npm "$@";; *) return 1;; esac
}
test "$(npm prefix --global)" = /usr
cage_install_tool claude
cage_install_tool claude
grep -qx 'fund=false' "$HOME/.npmrc"
test "$(cat "$HOME/global-npmrc")" = $'prefix=/usr\nfetch-retries=1'
if [ "$TEST_INSTALL" = npm ]; then
  grep -qx "prefix=$HOME/.local" "$HOME/.npmrc"
  test "$(npm prefix --global)" = "$HOME/.local"
  test "$(cat "$HOME/config-writes")" = config
else
  test "$(cat "$HOME/.npmrc")" = 'fund=false'
  test "$(npm prefix --global)" = /usr
  test ! -e "$HOME/config-writes"
fi
`)
			cmd.Env = append(os.Environ(), "HOME="+dir, "TEST_INSTALL="+install)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("existing %s installation: %v\n%s", install, err, out)
			}
		})
	}
}

func TestGeneratedGuestScriptsParseAsBash(t *testing.T) {
	for name, script := range map[string]string{"start": DefaultConfig().StartScript(), "tools": ToolsUpdateScript(), "first-install-functions": ToolInstallPrelude()} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command("bash", "-n")
			cmd.Stdin = strings.NewReader(script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
		})
	}
}

func TestChromiumPrefetchFallsBackButPropagatesCancellation(t *testing.T) {
	for _, status := range []int{1, 143} {
		cmd := exec.Command("bash", "-c", fmt.Sprintf("set -e\nsudo() { cat >/dev/null; return %d; }\n", status)+ToolInstallPrelude()+"cage_cache_chromium --only-upgrade chromium\necho normal-apt")
		out, err := cmd.CombinedOutput()
		fellBack := err == nil && strings.Contains(string(out), "normal-apt")
		if (status == 1 && !fellBack) || (status == 143 && (err == nil || cmd.ProcessState.ExitCode() != 143)) {
			t.Fatalf("prefetch exit %d: %v %s", status, err, out)
		}
	}
}

func TestPinnedToolInstallerRejectsWrongHashBeforeInstalling(t *testing.T) {
	dir := t.TempDir()
	commands := map[string]string{
		"flock":       "#!/bin/sh\nexit 0\n",
		"dpkg":        "#!/bin/sh\nif [ \"$1\" = --print-architecture ]; then echo arm64; else exit 1; fi\n",
		"code-server": "#!/bin/sh\necho 1.0.0\n",
		"curl":        "#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do if [ \"$1\" = -o ]; then shift; printf altered-package > \"$1\"; exit; fi; shift; done\nexit 1\n",
		"sha256sum":   "#!/bin/sh\ncat >/dev/null\nexit 1\n",
		"sudo":        "#!/bin/sh\nprintf attempted > \"$AUDIT_INSTALL\"\nexit 0\n",
	}
	for name, content := range commands {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0755); err != nil {
			t.Fatal(err)
		}
	}
	installed := filepath.Join(dir, "installed")
	cmd := exec.Command("bash", "-c", ToolInstallPrelude()+"\ncage_install_tool code-server")
	cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+dir+":"+os.Getenv("PATH"), "AUDIT_INSTALL="+installed)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "checksum mismatch") {
		t.Fatalf("changed download accepted: %v %s", err, out)
	}
	if _, err := os.Stat(installed); !os.IsNotExist(err) {
		t.Fatal("installer ran before verifying the artifact")
	}
}

func TestPinnedToolVersionIgnoresFirstRunNotices(t *testing.T) {
	for _, version := range []string{"4.144.0", "v4.144.1", "4.144.0-rc.1"} {
		t.Run(version, func(t *testing.T) {
			dir := t.TempDir()
			commands := map[string]string{
				"code-server": "#!/bin/sh\nprintf '%s\\n' '[2026-11-05T14:58:49.600Z] info Wrote default config file' '" + version + " commit with Code 1.144.0'\n",
				"dpkg":        "#!/bin/sh\nif [ \"$1\" = --print-architecture ]; then echo arm64; else test \"$2\" = \"${EXPECTED_VERSION#v}\"; fi\n",
				"curl":        "#!/bin/sh\necho unexpected-download >&2\nexit 90\n",
			}
			for name, body := range commands {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("bash", "-c", ToolInstallPrelude()+"\ncage_install_tool code-server")
			cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+dir+":"+os.Getenv("PATH"), "EXPECTED_VERSION="+version)
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "already meets") {
				t.Fatalf("%v: %s", err, out)
			}
		})
	}
}

func TestToolDownloadKeepsPartialAndResumes(t *testing.T) {
	dir := t.TempDir()
	commands := map[string]string{
		"sleep": "#!/bin/sh\nexit 0\n",
		"curl": `#!/bin/sh
while [ "$#" -gt 0 ]; do
  case "$1" in -o) shift; artifact="$1";; --continue-at) shift; test "$1" = - || exit 99;; esac
  shift
done
if [ ! -f "$HOME/recover" ]; then
  printf partial > "$artifact"
  exit 56
fi
test "$(cat "$artifact")" = partial || exit 98
printf complete > "$artifact"
printf 206
`,
		"sha256sum": "#!/bin/sh\nread checksum artifact\ntest \"$(cat \"$artifact\")\" = complete\n",
	}
	for name, body := range commands {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}
	run := func() ([]byte, error) {
		cmd := exec.Command("bash", "-c", ToolInstallPrelude()+`cage_download_tool https://example.invalid/tool pinned "$HOME/package"`)
		cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+dir+":"+os.Getenv("PATH"))
		return cmd.CombinedOutput()
	}
	if out, err := run(); err == nil || !strings.Contains(string(out), "partial data is saved") {
		t.Fatalf("%v %s", err, out)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "package.part")); err != nil || string(data) != "partial" {
		t.Fatal("partial lost", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "recover"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := run(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "package")); err != nil || string(data) != "complete" {
		t.Fatal("not published", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "package.part")); !os.IsNotExist(err) {
		t.Fatal("partial left after success")
	}
}

func TestStartClearsInheritedAccountComment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the start script runs in the Linux guest")
	}
	for _, test := range []struct{ comment, want string }{{"Host Person,,,", "usermod -c  worker\n"}, {"", ""}} {
		dir := t.TempDir()
		log := filepath.Join(dir, "calls")
		// Stand-ins observe the account change without touching this machine's accounts.
		for name, body := range map[string]string{
			"id":     "#!/bin/sh\n[ \"$1\" = -un ] && echo worker\n",
			"getent": fmt.Sprintf("#!/bin/sh\n[ \"$1 $2\" = 'passwd worker' ] && echo 'worker:x:1000:1000:%s:/home/worker:/bin/bash'\n", test.comment),
			"sudo":   fmt.Sprintf("#!/bin/sh\n[ \"$1\" = usermod ] || exit 91\necho \"$*\" >> %q\n", log),
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0755); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("bash", "-c", "set -euo pipefail\n"+clearAccountComment+"printf continued")
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		if err != nil || string(out) != "continued" {
			t.Fatalf("%q: %v %s", test.comment, err, out)
		}
		calls, _ := os.ReadFile(log)
		if string(calls) != test.want {
			t.Fatalf("comment %q made calls %q", test.comment, calls)
		}
	}
	if !strings.HasPrefix(componentStartScript(), clearAccountComment) {
		t.Fatal("component start does not clear the account comment")
	}
}

func TestGuestManifestInterruptedStartThenRetryPublishesManifest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the start script runs in the Linux guest")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	dir := t.TempDir()
	etcDir := filepath.Join(dir, "etc")
	if err := os.MkdirAll(etcDir, 0700); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(etcDir, "components.json")
	if err := os.WriteFile(manifestPath, []byte(`{"protocol":1,"revision":"stale"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte("#!/bin/sh\nexec \"$@\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(dir, "body-started")
	interrupted := strings.ReplaceAll(markGuestStart("touch \"$STARTED_FLAG\"; sleep 30"), "/etc/claude-cage/", etcDir+"/")
	cmd := exec.Command("bash", "-c", interrupted)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "STARTED_FLAG="+started)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("guest start body did not begin")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	// The last manifest is kept, but the durable marker says it is not current.
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("manifest removed by SIGKILL mid-start: %v", err)
	}
	if _, err := os.Stat(filepath.Join(etcDir, "components.updating")); err != nil {
		t.Fatalf("interrupted start left no updating marker: %v", err)
	}
	retry := strings.ReplaceAll(markGuestStart("printf recovered"), "/etc/claude-cage/", etcDir+"/")
	retryCmd := exec.Command("bash", "-c", retry)
	retryCmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
	if out, err := retryCmd.CombinedOutput(); err != nil {
		t.Fatalf("retry after SIGKILL failed: %v %s", err, out)
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var got GuestManifest
	if err := json.Unmarshal(raw, &got); err != nil || !got.Current() {
		t.Fatalf("republished manifest invalid: %q %v", raw, err)
	}
	entries, err := os.ReadDir(etcDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("unexpected temp files left in manifest dir: %v %v", entries, err)
	}
}

func TestGuestStartRecoversFromTruncatedGuestFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the start script runs in the Linux guest")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	// 1. code-server.yaml and browser-password guard against empty/truncated files.
	guardStart := strings.Index(guestStart, `mkdir -p "$HOME/.config/claude-cage"`)
	guardEnd := strings.Index(guestStart, `sudo install -d -o cage-browser`)
	if guardStart < 0 || guardEnd <= guardStart {
		t.Fatal("credential guard section not found in guest-start.sh")
	}
	guardScript := "set -euo pipefail\n" + guestStart[guardStart:guardEnd]
	for name, badYAML := range map[string]string{"empty": "", "truncated": "auth: password\n"} {
		t.Run("credentials-"+name, func(t *testing.T) {
			home := t.TempDir()
			cfgDir := filepath.Join(home, ".config", "claude-cage")
			if err := os.MkdirAll(cfgDir, 0700); err != nil {
				t.Fatal(err)
			}
			yamlPath := filepath.Join(cfgDir, "code-server.yaml")
			passPath := filepath.Join(cfgDir, "browser-password")
			if err := os.WriteFile(yamlPath, []byte(badYAML), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(passPath, nil, 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-c", guardScript)
			cmd.Env = append(os.Environ(), "HOME="+home)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v %s", err, out)
			}
			yamlBytes, err := os.ReadFile(yamlPath)
			if err != nil || !strings.Contains(string(yamlBytes), "password: ") {
				t.Fatalf("code-server.yaml not recovered: %q %v", yamlBytes, err)
			}
			passBytes, err := os.ReadFile(passPath)
			if err != nil || len(strings.TrimSpace(string(passBytes))) != 8 {
				t.Fatalf("browser-password not recovered: %q %v", passBytes, err)
			}
			// Re-entry preserves valid credentials.
			cmd2 := exec.Command("bash", "-c", guardScript)
			cmd2.Env = append(os.Environ(), "HOME="+home)
			if out, err := cmd2.CombinedOutput(); err != nil {
				t.Fatalf("re-entry: %v %s", err, out)
			}
			if again, _ := os.ReadFile(yamlPath); string(again) != string(yamlBytes) {
				t.Fatal("re-entry overwrote valid code-server.yaml")
			}
			if entries, _ := os.ReadDir(cfgDir); len(entries) != 2 {
				t.Fatalf("temp files left behind: %v", entries)
			}
		})
	}

	// 2. PYCONTROL recovers truncated/corrupt browser-control.json and preserves valid token.
	ctrlStart := strings.Index(guestStart, "import json, os, secrets, grp, tempfile")
	ctrlEnd := strings.Index(guestStart[ctrlStart:], "\nPYCONTROL")
	if ctrlStart < 0 || ctrlEnd < 0 {
		t.Fatal("PYCONTROL block not found")
	}
	ctrlScript := guestStart[ctrlStart : ctrlStart+ctrlEnd]
	ctrlScript = strings.ReplaceAll(ctrlScript, `Path('/etc/claude-cage/browser-control.json')`, `Path(os.environ["CTRL_PATH"])`)
	ctrlScript = strings.ReplaceAll(ctrlScript, `Path('/etc/claude-cage/display-scale')`, `Path(os.environ["SCALE_PATH"])`)
	ctrlScript = strings.ReplaceAll(ctrlScript, "grp.getgrnam('cage-browser').gr_gid", "None")
	ctrlScript = recordDirectoryFsyncs + ctrlScript + recordDirectoryFsyncsReport
	for _, tc := range []struct {
		name      string
		existing  string
		wantToken string
	}{
		{"empty", "", ""},
		{"truncated", `{"token":"abc`, ""},
		{"wrong-type", `[]`, ""},
		{"valid-preserved", `{"token":"keep-existing-token"}`, "keep-existing-token"},
	} {
		t.Run("browser-control-"+tc.name, func(t *testing.T) {
			dir := t.TempDir()
			home := filepath.Join(dir, "home")
			etcDir := filepath.Join(dir, "etc")
			if err := os.MkdirAll(filepath.Join(home, ".config", "claude-cage"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(etcDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, ".config", "claude-cage", "code-server.yaml"), []byte("auth: password\npassword: editor-secret\n"), 0600); err != nil {
				t.Fatal(err)
			}
			ctrlPath := filepath.Join(etcDir, "browser-control.json")
			scalePath := filepath.Join(etcDir, "display-scale")
			if err := os.WriteFile(ctrlPath, []byte(tc.existing), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("python3", "-c", ctrlScript)
			cmd.Env = append(os.Environ(),
				"CAGE_DISPLAY_SCALE=2",
				"CAGE_WORKSPACE=/work",
				"CAGE_USER_HOME="+home,
				"CTRL_PATH="+ctrlPath,
				"SCALE_PATH="+scalePath,
				"FSYNC_DIRS="+etcDir,
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v %s", err, out)
			}
			// display-scale, browser-control.json and desktop-downloads.json: each rename is flushed.
			if got := strings.Count(string(out), "dir-fsync "+etcDir+"\n"); got != 3 {
				t.Fatalf("directory fsyncs %d, want 3: %s", got, out)
			}
			raw, err := os.ReadFile(ctrlPath)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Token          string `json:"token"`
				Workspace      string `json:"workspace"`
				EditorPassword string `json:"editorPassword"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("invalid browser-control.json: %q %v", raw, err)
			}
			if got.Token == "" || got.Workspace != "/work" || got.EditorPassword != "editor-secret" {
				t.Fatalf("unexpected browser-control.json: %+v", got)
			}
			if tc.wantToken != "" && got.Token != tc.wantToken {
				t.Fatalf("valid token replaced: got %q want %q", got.Token, tc.wantToken)
			}
			if entries, _ := os.ReadDir(etcDir); len(entries) != 3 {
				t.Fatalf("temp files left in etcDir: %v", entries)
			}
			downloadsRaw, err := os.ReadFile(filepath.Join(etcDir, "desktop-downloads.json"))
			if err != nil {
				t.Fatal(err)
			}
			var downloadsCfg struct {
				Workspace string `json:"workspace"`
			}
			if json.Unmarshal(downloadsRaw, &downloadsCfg) != nil || downloadsCfg.Workspace != "/work" {
				t.Fatalf("unexpected desktop-downloads.json: %q", downloadsRaw)
			}
		})
	}

	// 3. PYCODE recovers truncated/corrupt Chromium Preferences atomically.
	prefStart := strings.Index(guestStart, "import json, os\nfrom pathlib import Path\np = Path.home()")
	prefEnd := strings.Index(guestStart[prefStart:], "\nPYCODE")
	prefScript := strings.Replace(guestStart[prefStart:prefStart+prefEnd], "Path.home()", `Path(os.environ["FIXTURE"])`, 1)
	prefScript = recordDirectoryFsyncs + prefScript + recordDirectoryFsyncsReport
	for name, corrupt := range map[string]string{"empty": "", "truncated": `{"intl":`, "wrong-type": `"bad"`} {
		t.Run("preferences-"+name, func(t *testing.T) {
			dir := t.TempDir()
			prof := filepath.Join(dir, "profile", "Default")
			if err := os.MkdirAll(prof, 0700); err != nil {
				t.Fatal(err)
			}
			prefs := filepath.Join(prof, "Preferences")
			if err := os.WriteFile(prefs, []byte(corrupt), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("python3", "-c", prefScript)
			cmd.Env = append(os.Environ(), "FIXTURE="+dir, "CAGE_LANGUAGE=en-US", "FSYNC_DIRS="+prof)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v %s", err, out)
			}
			if !strings.Contains(string(out), "dir-fsync "+prof+"\n") {
				t.Fatalf("the rename was not flushed: %s", out)
			}
			raw, err := os.ReadFile(prefs)
			if err != nil || !json.Valid(raw) {
				t.Fatalf("preferences not recovered: %q %v", raw, err)
			}
			entries, _ := os.ReadDir(prof)
			if corrupt == "" {
				// Nothing worth keeping in an empty file.
				if len(entries) != 1 {
					t.Fatalf("temp files left in profile dir: %v", entries)
				}
				return
			}
			// The damaged file is kept beside the new one, unchanged.
			if len(entries) != 2 || !strings.HasPrefix(entries[1].Name(), "Preferences.corrupt-") {
				t.Fatalf("damaged preferences not kept: %v", entries)
			}
			if kept, _ := os.ReadFile(filepath.Join(prof, entries[1].Name())); string(kept) != corrupt {
				t.Fatalf("backup %q, want %q", kept, corrupt)
			}
		})
	}
}

// recordDirectoryFsyncs wraps os.fsync in a guest-start.sh Python block to
// note each directory it flushes; recordDirectoryFsyncsReport prints
// "dir-fsync PATH" once per flush of a directory listed in FSYNC_DIRS.
const recordDirectoryFsyncs = `import os as _os, stat as _stat
_fsyncs = []
_fsync = _os.fsync
def _record(fd):
    info = _os.fstat(fd)
    if _stat.S_ISDIR(info.st_mode):
        _fsyncs.append((info.st_dev, info.st_ino))
    return _fsync(fd)
_os.fsync = _record
`

const recordDirectoryFsyncsReport = `
for _path in _os.environ.get("FSYNC_DIRS", "").split(_os.pathsep):
    if _path:
        _info = _os.stat(_path)
        for _flushed in _fsyncs:
            if _flushed == (_info.st_dev, _info.st_ino):
                print("dir-fsync " + _path)
`
