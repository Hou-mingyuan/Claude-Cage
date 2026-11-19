package env

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sylvinus/agent-vm/internal/paths"
)

// Exercise the drive walk on every host and a real NTFS junction on Windows.
func TestProjectRelWindows(t *testing.T) {
	old := paths.Windows
	paths.Windows = func() bool { return true }
	t.Cleanup(func() { paths.Windows = old })
	root := "C:/"
	temp := t.TempDir()
	if runtime.GOOS == "windows" {
		root = paths.Host(temp) + "/"
	} else {
		t.Chdir(temp)
	}
	project, alias := root+"p", root+"link"
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		// A junction requires neither Developer Mode nor symlink privilege.
		cmd := exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"),
			"-NoProfile", "-NonInteractive", "-Command",
			"$ErrorActionPreference='Stop'; New-Item -ItemType Junction -Path $env:CAGE_TEST_ALIAS -Target $env:CAGE_TEST_PROJECT | Out-Null")
		cmd.Env = append(os.Environ(), "CAGE_TEST_ALIAS="+alias, "CAGE_TEST_PROJECT="+project)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("create junction: %v: %s", err, output)
		}
	} else if err := os.Symlink("p", alias); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(alias); err != nil {
			t.Error(err)
		}
	})
	for target, want := range map[string]string{project + "/.env": ".env", alias + "/.env": ".env", alias + "/sub/x": "sub/x"} {
		if rel, ok := ProjectRel(project, target); !ok || rel != want {
			t.Errorf("ProjectRel(%q, %q) = %q, %v", project, target, rel, ok)
		}
	}
	if _, ok := ProjectRel(project, root+"other/.env"); ok {
		t.Error("other folder is not in the project")
	}
}
