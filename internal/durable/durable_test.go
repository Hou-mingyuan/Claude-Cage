package durable

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSyncDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SyncDir(dir); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing")
	if err := SyncDir(missing); (err == nil) != (runtime.GOOS == "windows") {
		t.Fatalf("missing directory: %v", err)
	}
}
