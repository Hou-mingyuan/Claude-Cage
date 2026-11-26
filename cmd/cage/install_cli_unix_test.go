//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallTerminalCommandPreservesUserShellAndExistingCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("PATH", t.TempDir())
	profile := filepath.Join(home, ".profile")
	const existing = "export PROJECT_SETTING=active\n"
	if err := os.WriteFile(profile, []byte(existing), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if code := installTerminalCommand(); code != 0 {
			t.Fatal(code)
		}
	}
	contents, err := os.ReadFile(profile)
	if err != nil || !strings.HasPrefix(string(contents), existing) || strings.Count(string(contents), ".local/bin") != 1 {
		t.Fatalf("shell profile: %s %v", contents, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".bash_profile")); !os.IsNotExist(err) {
		t.Fatal("installed a new profile shadowing the user's .profile")
	}
	link := filepath.Join(home, ".local", "bin", "cage")
	target, err := filepath.EvalSymlinks(link)
	exe, _ := os.Executable()
	real, _ := filepath.EvalSymlinks(exe)
	if err != nil || target != real {
		t.Fatalf("link %q %v", target, err)
	}
	t.Setenv("PATH", filepath.Dir(link))
	if code := installTerminalCommand(); code != 0 {
		t.Fatal(code)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte("existing executable"), 0700); err != nil {
		t.Fatal(err)
	}
	if code := installTerminalCommand(); code == 0 {
		t.Fatal("claimed an existing command")
	}
	if got, err := os.ReadFile(link); err != nil || string(got) != "existing executable" {
		t.Fatal("existing executable changed")
	}
}
