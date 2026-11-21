package workbench

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Set CAGE_TEST_LOCAL_CODEX to exercise the installed CLI and model.
func TestLocalCodexConversation(t *testing.T) {
	if os.Getenv("CAGE_TEST_LOCAL_CODEX") != "1" {
		t.Skip("requires installed Codex and a signed-in account")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	want := hex.EncodeToString(nonce[:])
	if err := os.WriteFile(filepath.Join(root, "marker.txt"), []byte(want), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	session, err := startCodexConversation(ctx, localCodex(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer session.close()
	first, err := session.ask("Read marker.txt in the assigned project. Reply with only its contents.")
	if err != nil || strings.TrimSpace(first) != want {
		t.Fatalf("first answer matched=%t, error=%v", first == want, err)
	}
	second, err := session.ask("Without reading any files again, reply with only the last four characters of your previous answer.")
	if err != nil || strings.TrimSpace(second) != want[len(want)-4:] {
		t.Fatalf("follow-up matched=%t, error=%v", second == want[len(want)-4:], err)
	}
}

func TestCodexAppServerArgsSelectDefaultPermissions(t *testing.T) {
	for _, tc := range []struct {
		computerUse bool
		want        string
	}{{false, `default_permissions="delegated"`}, {true, `sandbox_mode="workspace-write"`}} {
		args := codexAppServerArgs(t.TempDir(), tc.computerUse)
		found := false
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-c" && args[i+1] == tc.want {
				found = true
			}
		}
		if !found {
			t.Fatalf("computerUse=%v: %q missing from %q", tc.computerUse, tc.want, args)
		}
		if n := len(args); n < 3 || strings.Join(args[n-3:], " ") != "app-server --listen stdio://" {
			t.Fatalf("computerUse=%v: app-server must be the last command: %q", tc.computerUse, args)
		}
	}
}
