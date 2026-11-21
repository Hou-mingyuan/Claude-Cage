package host

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sylvinus/agent-vm/internal/durable"
)

func TestPrivateStorageProtectsNewAndReplacedRecords(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "credential")
	if err := os.WriteFile(path, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ProtectFile(path); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivateFile(path); err != nil {
		t.Fatal(err)
	}
	replacement, err := os.CreateTemp(dir, ".replacement-")
	if err != nil {
		t.Fatal(err)
	}
	if err := ProtectFile(replacement.Name()); err != nil {
		t.Fatal(err)
	}
	replacement.Close()
	if err := os.Rename(replacement.Name(), path); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivateFile(path); err != nil {
		t.Fatalf("atomic replacement lost private permissions: %v", err)
	}
	if err := CheckPrivateFile(dir); err == nil {
		t.Fatal("directory accepted as credential record")
	}
}

func TestCreatePrivateFileNeverReplacesOrLeavesPartialFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := CreatePrivateFile(path, []byte("first\n")); err != nil {
		t.Fatal(err)
	}
	if err := CreatePrivateFile(path, []byte("second\n")); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing file: %v", err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "first\n" {
		t.Fatalf("existing content changed: %q %v", b, err)
	}
	if err := CheckPrivateFile(path); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v %v", entries, err)
	}
}

func TestCreatePrivateFileFlushesTheDirectory(t *testing.T) {
	dir := t.TempDir()
	var synced []string
	original := durable.SyncDir
	durable.SyncDir = func(d string) error { synced = append(synced, d); return original(d) }
	t.Cleanup(func() { durable.SyncDir = original })
	if err := CreatePrivateFile(filepath.Join(dir, "record"), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if len(synced) != 1 || filepath.Clean(synced[0]) != dir {
		t.Fatalf("synced %q", synced)
	}
}
