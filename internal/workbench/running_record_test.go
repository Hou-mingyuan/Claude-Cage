package workbench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestVMRecordWithReusedPIDIsNotRunning(t *testing.T) {
	for _, guest := range []string{"linux"} {
		t.Run(guest, func(t *testing.T) {
			s := testServer(t)
			s.preferences.Workspace, _ = filepath.EvalSymlinks(t.TempDir())
			dir := s.instanceDir()
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadConfig(s.configPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.RecordRunningAt(dir); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "cage-running.json")
			var record map[string]any
			b, _ := os.ReadFile(path)
			if err := json.Unmarshal(b, &record); err != nil {
				t.Fatal(err)
			}
			started, _ := record["started"].(string)
			if started == "" {
				t.Skip("process start time is unavailable on this platform")
			}
			if !s.vmRunning() {
				t.Fatal("current owner was not recognized")
			}
			if owners, err := controllerVMOwners(s.profile); err != nil || len(owners) != 1 {
				t.Fatalf("owners = %v, %v", owners, err)
			}
			record["started"] = started + "0"
			if err := writePrivate(path, record); err != nil {
				t.Fatal(err)
			}
			if s.vmRunning() {
				t.Fatal("record from an earlier process with the same PID was treated as running")
			}
			if owners, err := controllerVMOwners(s.profile); err != nil || len(owners) != 0 {
				t.Fatalf("stale owners = %v, %v", owners, err)
			}
			delete(record, "started")
			if err := writePrivate(path, record); err != nil {
				t.Fatal(err)
			}
			if !s.vmRunning() {
				t.Fatal("legacy PID-only record was rejected")
			}
		})
	}
}

func TestUnreadableVMRecordNamesNoOwner(t *testing.T) {
	s := testServer(t)
	dir := filepath.Join(s.profile, "state", "lima", "interrupted")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"", `{"pid":`} {
		if err := os.WriteFile(filepath.Join(dir, "cage-running.json"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if owners, err := controllerVMOwners(s.profile); err != nil || len(owners) != 0 {
			t.Fatalf("record %q: owners = %v, %v", content, owners, err)
		}
		if controllerHasVM(s.profile) {
			t.Fatalf("record %q reported a running environment", content)
		}
	}
}

// A complete record without a usable PID is refused rather than read as no
// owner: only an empty or cut-off file comes from an interrupted write.
func TestValidVMRecordWithoutPIDIsRefused(t *testing.T) {
	s := testServer(t)
	dir := filepath.Join(s.profile, "state", "lima", "damaged")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{`{"pid":0}`, `{"pid":-1,"started":"x"}`, `{}`, `{"pid":"12"}`} {
		if err := os.WriteFile(filepath.Join(dir, "cage-running.json"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if owners, err := controllerVMOwners(s.profile); err == nil {
			t.Fatalf("record %q: owners = %v, want an error", content, owners)
		}
	}
}
