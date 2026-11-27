package managedproxy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func stalePreflightDir(t *testing.T, parent, name string, record *preflightCoreRecord) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := lockPreflightDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The owner is gone. Unlock explicitly: Windows releases the lock of a
	// closed handle asynchronously, after the sweep below may have run.
	_ = unlockPreflight(lock)
	_ = lock.Close()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if record != nil {
		data, _ := json.Marshal(record)
		if err := Write(filepath.Join(dir, preflightRecordName), data); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

func TestSweepPreflightRemovesOnlyStaleOwnedDirectories(t *testing.T) {
	parent := t.TempDir()
	record := preflightCoreRecord{PID: 4242, Started: 99, BootID: "boot"}
	stale := stalePreflightDir(t, parent, preflightDirPrefix+"stale", &record)
	noRecord := stalePreflightDir(t, parent, preflightDirPrefix+"norecord", nil)

	live := filepath.Join(parent, preflightDirPrefix+"live")
	if err := os.Mkdir(live, 0700); err != nil {
		t.Fatal(err)
	}
	owner, err := lockPreflightDir(live)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()

	young := filepath.Join(parent, preflightDirPrefix+"young")
	old := filepath.Join(parent, preflightDirPrefix+"old")
	for _, dir := range []string{young, old} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * preflightLockGrace)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	other := filepath.Join(parent, "account-policy")
	if err := os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}
	otherFile := filepath.Join(parent, "config.yaml")
	if err := os.WriteFile(otherFile, nil, 0600); err != nil {
		t.Fatal(err)
	}

	var stopped []preflightCoreRecord
	if err := sweepPreflight(parent, func(r preflightCoreRecord) error {
		stopped = append(stopped, r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(stopped) != 1 || stopped[0] != record {
		t.Fatalf("stopped %+v, want only %+v", stopped, record)
	}
	for _, dir := range []string{stale, noRecord, old} {
		if exists(dir) {
			t.Fatalf("stale %s kept", filepath.Base(dir))
		}
	}
	for _, path := range []string{live, filepath.Join(live, preflightLockName), young, other, otherFile} {
		if !exists(path) {
			t.Fatalf("%s removed", filepath.Base(path))
		}
	}
}

func TestSweepPreflightKeepsDirectoryWhenCoreCannotBeStopped(t *testing.T) {
	parent := t.TempDir()
	dir := stalePreflightDir(t, parent, preflightDirPrefix+"a", &preflightCoreRecord{PID: 4242, Started: 1})
	err := sweepPreflight(parent, func(preflightCoreRecord) error { return errors.New("busy") })
	if err == nil || !exists(filepath.Join(dir, preflightRecordName)) {
		t.Fatalf("err=%v, record kept=%v", err, exists(filepath.Join(dir, preflightRecordName)))
	}
}

func TestSweepPreflightNeverFollowsSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need developer mode on Windows")
	}
	parent, outside := t.TempDir(), t.TempDir()
	victim := filepath.Join(outside, "keep")
	if err := os.WriteFile(victim, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	// A preflight-named link to a directory is not swept at all.
	if err := os.Symlink(outside, filepath.Join(parent, preflightDirPrefix+"link")); err != nil {
		t.Fatal(err)
	}
	// A link inside a stale directory is removed, not traversed.
	stale := stalePreflightDir(t, parent, preflightDirPrefix+"stale", nil)
	if err := os.Symlink(outside, filepath.Join(stale, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := sweepPreflight(parent, func(preflightCoreRecord) error { t.Fatal("unexpected stop"); return nil }); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(parent, preflightDirPrefix+"link")) || !exists(victim) {
		t.Fatal("symlink or its target was removed")
	}
	if exists(stale) {
		t.Fatal("stale directory kept")
	}
}

func TestProcargsExecutable(t *testing.T) {
	raw := append([]byte{2, 0, 0, 0}, "/Applications/X.app/mihomo\x00\x00\x00mihomo\x00-d\x00"...)
	if got := procargsExecutable(raw); got != "/Applications/X.app/mihomo" {
		t.Fatalf("got %q", got)
	}
	for _, raw := range [][]byte{nil, {1, 0, 0}, {1, 0, 0, 0, 'a'}} {
		if got := procargsExecutable(raw); got != "" {
			t.Fatalf("got %q for %v", got, raw)
		}
	}
}
