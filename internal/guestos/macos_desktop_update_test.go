package guestos

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Execute the migration with isolated application/data directories. Only the
// macOS identity and privileged command adapters are replaced; the copy,
// verification order, publication and cleanup are the production shell flow.
func TestMacOSDesktopUpdateMigration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("macOS installer fixture requires POSIX commands")
	}
	source, err := os.ReadFile("../../scripts/guest-os/claude-desktop-macos.sh")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(source), "app=/Applications/Claude.app")
	end := strings.Index(string(source), "\nreport() {")
	if start < 0 || end < start {
		t.Fatal("migration entry unavailable")
	}
	for _, scenario := range []string{"success", "copy-failure", "signature-failure", "publish-failure", "running", "source-newer", "source-older"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			apps := filepath.Join(dir, "Applications")
			old := filepath.Join(apps, "Claude.app")
			personal := filepath.Join(dir, "Users", "alice", "Library", "Application Support", "Claude", "login")
			if err := os.MkdirAll(old, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(personal), 0700); err != nil {
				t.Fatal(err)
			}
			for path, content := range map[string]string{filepath.Join(old, "verified"): "official app", personal: "preserved login"} {
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "source-newer" || scenario == "source-older" {
				current := filepath.Join(apps, "Claude", "Claude.app")
				if err := os.MkdirAll(current, 0755); err != nil {
					t.Fatal(err)
				}
				incoming, installed := "2.0", "1.0"
				if scenario == "source-older" {
					incoming, installed = installed, incoming
				}
				for path, content := range map[string]string{filepath.Join(old, "version"): incoming, filepath.Join(current, "version"): installed, filepath.Join(current, "verified"): "previous app"} {
					if err := os.WriteFile(path, []byte(content), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0755); err != nil {
				t.Fatal(err)
			}
			for name, body := range map[string]string{
				"id":        `if [ "$1" = -u ]; then echo 501; else echo 20; fi`,
				"dscl":      `echo "NFSHomeDirectory: $TEST_ROOT/Users/alice"`,
				"stat":      `echo 501`,
				"plutil":    `if [ "$2" = CFBundleVersion ]; then for value; do target=$value; done; cat "${target%/Contents/Info.plist}/version"; else echo com.anthropic.claudefordesktop; fi`,
				"pgrep":     `test "$TEST_SCENARIO" = running`,
				"install":   `for value; do target=$value; done; mkdir -p "$target"`,
				"sudo":      `shift 2; exec "$@"`,
				"ditto":     `test "$TEST_SCENARIO" != copy-failure || exit 1; cp -R "$1" "$2"`,
				"codesign":  `for value; do target=$value; done; test -f "$target/verified" || exit 1; if [ "$TEST_SCENARIO" = signature-failure ] && [ "$target" != "$TEST_ROOT/Applications/Claude.app" ]; then exit 1; fi`,
				"osascript": `test "$TEST_SCENARIO" != publish-failure || exit 1; shift 3; if [ "$3" = 4 ]; then mv -n "$1" "$2"; else mv "$2" "$1.previous"; mv "$1" "$2"; mv "$1.previous" "$1"; fi`,
			} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			script := string(source)[start:end]
			script = strings.ReplaceAll(script, "/Applications", apps)
			script = strings.ReplaceAll(script, "/Users", filepath.Join(dir, "Users"))
			script = strings.ReplaceAll(script, "[[ $EUID == 0 ]]", "true")
			script = strings.ReplaceAll(script, "/usr/bin/osascript", "osascript")
			script = "set -euo pipefail\nfail() { echo \"$*\" >&2; exit 1; }\n" + script
			run := func() ([]byte, error) {
				cmd := exec.Command("bash", "-s", "--", "migrate", "alice")
				cmd.Stdin = strings.NewReader(script)
				cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "TEST_ROOT="+dir, "TEST_SCENARIO="+scenario)
				return cmd.CombinedOutput()
			}
			out, err := run()
			destination := filepath.Join(apps, "Claude", "Claude.app", "verified")
			if scenario == "success" || scenario == "source-newer" || scenario == "source-older" {
				if err != nil {
					t.Fatalf("migration: %v %s", err, out)
				}
				if _, err := os.Stat(destination); err != nil {
					t.Fatal("verified app not published", err)
				}
				want := "official app"
				if scenario == "source-older" {
					want = "previous app"
				}
				if data, err := os.ReadFile(destination); err != nil || string(data) != want {
					t.Fatalf("selected wrong app version: %q, %v", data, err)
				}
				if _, err := os.Stat(old); !os.IsNotExist(err) {
					t.Fatal("old app copy remains")
				}
				if out, err := run(); err != nil {
					t.Fatalf("repeated migration: %v %s", err, out)
				}
			} else {
				if err == nil {
					t.Fatal("unsafe migration accepted")
				}
				if data, err := os.ReadFile(filepath.Join(old, "verified")); err != nil || string(data) != "official app" {
					t.Fatal("failed migration damaged original app")
				}
				if _, err := os.Stat(destination); !os.IsNotExist(err) {
					t.Fatal("failed migration published app")
				}
			}
			if data, err := os.ReadFile(personal); err != nil || string(data) != "preserved login" {
				t.Fatal("migration changed personal data")
			}
		})
	}
}
