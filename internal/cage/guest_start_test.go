package cage

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuestStartMissingToolRecovery(t *testing.T) {
	for _, tool := range []string{"chromium", "code-server", "Xtigervnc", "openbox", "websockify"} {
		t.Run(tool, func(t *testing.T) {
			chromium := filepath.Join(t.TempDir(), "chromium")
			if tool != "chromium" {
				if err := os.WriteFile(chromium, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			// Simulate installed commands without running guest services on the host.
			script := `command() {
  if [[ "$1" == -v ]]; then [[ "$2" != "$MISSING_TOOL" ]]; else builtin command "$@"; fi
}
sudo() { echo 'unexpected privileged action' >&2; exit 91; }
` + strings.ReplaceAll(guestStart, "/usr/bin/chromium", filepath.ToSlash(chromium))
			cmd := exec.Command("bash", "-c", script)
			cmd.Env = append(os.Environ(), "MISSING_TOOL="+tool)
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "Missing "+tool) {
				t.Fatalf("missing tool did not stop startup: %v %s", err, out)
			}
			for _, want := range []string{"older base", "cage up --reset", "disk", "lost"} {
				if !strings.Contains(string(out), want) {
					t.Errorf("recovery lacks %q: %s", want, out)
				}
			}
			if (tool == "chromium" || tool == "code-server") && !strings.Contains(string(out), "cage update-tools") {
				t.Errorf("missing tool has no in-place recovery: %s", out)
			}
		})
	}
}
