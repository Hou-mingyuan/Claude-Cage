package cage

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestInstallTLS12PreludeOptInAndCleanup(t *testing.T) {
	if got := InstallTLS12Prelude(false); got != "" {
		t.Fatalf("TLS 1.2 compatibility must be off by default, got: %q", got)
	}
	if strings.Contains(ToolsUpdateScript(), "tls-max") || strings.Contains(ToolsUpdateScript(), "CURL_HOME") {
		t.Fatal("default ToolsUpdateScript must not force TLS 1.2")
	}
	prelude := InstallTLS12Prelude(true)
	if !strings.Contains(prelude, `tls-max = "1.2"`) || !strings.Contains(prelude, "CURL_HOME") {
		t.Fatalf("opt-in prelude missing curl TLS 1.2 configuration: %q", prelude)
	}
	if strings.Contains(strings.ToLower(prelude), "cage") {
		t.Fatalf("install curl prelude must not expose project identity: %q", prelude)
	}
	if !strings.Contains(ToolsUpdateScriptFor(true), `tls-max = "1.2"`) {
		t.Fatal("ToolsUpdateScriptFor(true) missing opt-in TLS 1.2 prelude")
	}
	dir := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "bash", "-c", "set -euo pipefail\n"+prelude+`printf '%s\n' "$CURL_HOME"; cat "$CURL_HOME/.curlrc"`)
	cmd.Env = append(os.Environ(), "HOME="+dir, "TMPDIR="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash execution failed: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 || lines[1] != `tls-max = "1.2"` {
		t.Fatalf("unexpected curlrc output: %q", out)
	}
	if _, err := os.Stat(lines[0]); !os.IsNotExist(err) {
		t.Fatalf("temporary CURL_HOME %q was not cleaned up on exit: %v", lines[0], err)
	}
}

// The TLS 1.2 cap lives in CURL_HOME, which sudo's env_reset drops. Every curl
// Cage runs during installation must therefore run as the invoking user, in
// the shell that exported CURL_HOME: never on a sudo command line or inside a
// script that sudo runs (a heredoc fed to a sudo shell).
func TestInstallCurlNeverRunsUnderSudo(t *testing.T) {
	setup, err := os.ReadFile("../../agent-vm.setup.sh")
	if err != nil {
		t.Fatal(err)
	}
	scripts := map[string]string{
		"tools update":   ToolsUpdateScriptFor(true),
		"tool prelude":   ToolInstallPrelude(),
		"claude desktop": ClaudeDesktopInstallScript(),
		"guest setup":    GuestSetup,
		"debian prepare": DebianPrepareScript,
		"setup script":   string(setup),
	}
	if offenders := curlUnderSudo("sudo curl -fsSL https://example.com -o /tmp/x\nsudo env A=1 sh -s <<'X'\ncurl https://example.com\nX\n"); len(offenders) != 2 {
		t.Fatalf("checker missed sudo curl: %v", offenders)
	}
	for name, script := range scripts {
		if offenders := curlUnderSudo(script); len(offenders) > 0 {
			t.Errorf("%s runs curl under sudo, outside the TLS 1.2 CURL_HOME cap: %q", name, offenders)
		}
	}
}

func curlUnderSudo(script string) []string {
	download := regexp.MustCompile(`\b(curl|cage_curl_download|cage_download_tool)\b`)
	heredoc := regexp.MustCompile(`<<-?\s*'?"?([A-Za-z_][A-Za-z0-9_]*)'?"?`)
	var offenders []string
	terminator := ""
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if terminator != "" {
			if trimmed == terminator {
				terminator = ""
			} else if !strings.HasPrefix(trimmed, "#") && download.MatchString(trimmed) {
				offenders = append(offenders, trimmed)
			}
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		at := regexp.MustCompile(`(^|[\s;&|(])sudo\s`).FindStringIndex(trimmed)
		if at == nil {
			continue
		}
		command := trimmed[at[0]:]
		// Package names in an APT transaction are not downloads.
		if !strings.Contains(command, "apt-get") && download.MatchString(command) {
			offenders = append(offenders, trimmed)
		}
		if m := heredoc.FindStringSubmatch(command); m != nil && regexp.MustCompile(`\b(sh|bash|python3)\b`).MatchString(command) {
			terminator = m[1]
		}
	}
	return offenders
}
