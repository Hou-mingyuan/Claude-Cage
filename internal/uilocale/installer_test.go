package uilocale

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsInstallerSeedsOnlyNewPreference(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	if err := SeedWindowsInstallerChoice(profile, 1034); err != nil {
		t.Fatal(err)
	}
	if got := Load(profile).Language; got != "es" {
		t.Fatalf("new installer preference = %q", got)
	}
	path := filepath.Join(profile, PreferenceFile)
	if err := os.WriteFile(path, []byte("{\"language\":\"system\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SeedWindowsInstallerChoice(profile, 1036); err != nil {
		t.Fatal(err)
	}
	if got := Load(profile).Language; got != System {
		t.Fatalf("installer replaced existing application preference: %q", got)
	}
	if err := SeedWindowsInstallerChoice(profile, 0); err == nil {
		t.Fatal("invalid installer language accepted")
	}
}
