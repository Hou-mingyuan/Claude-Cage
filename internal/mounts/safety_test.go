package mounts

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/paths"
)

// What Lima cannot mount, or 0.2 refused: said why.
func TestUnmountable(t *testing.T) {
	for dir, want := range map[string]string{
		"/home/me/proj":     "",
		"/home/me/my proj":  "whitespace",
		"/home/me/a\tb":     "whitespace",
		`/home/me/a"b`:      "a quote",
		`/home/me/a\b`:      "a backslash",
		"/home/me/a\x01b":   "a control character",
		"/home/me/été/proj": "",
		"/home/me/R&D":      "",
		"/{{.Home}}":        "a template",
		"/home/me/a\xffb":   "not valid UTF-8",
		"/home/me/a\u2028b": "whitespace",
	} {
		why, bad := Unmountable(dir)
		if bad != (want != "") || !strings.Contains(why, want) {
			t.Errorf("%q: %q %v", dir, why, bad)
		}
	}
}

// A single file given as a symlink (stow's dotfiles) is staged as the file
// it names, in step with it: not as a link pointing out of the staging
// folder, which the VM could not follow.
func TestStageSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "gitconfig")
	os.MkdirAll(filepath.Dir(real), 0o755)
	os.WriteFile(real, []byte("v1"), 0o644)
	link := filepath.Join(dir, ".gitconfig")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlink unavailable: " + err.Error())
	}
	dst := filepath.Join(dir, "stage", "f")
	if !Stage(link, dst, io.Discard) {
		t.Fatal("not staged")
	}
	if fi, err := os.Lstat(dst); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("staged as a link: %v %v", fi, err)
	}
	os.WriteFile(real, []byte("v2"), 0o644)
	if b, _ := os.ReadFile(dst); string(b) != "v2" {
		t.Errorf("not in step with the file: %q", b)
	}
}

// Where the file system ignores case, the home folder in other capitals is
// the home folder.
func TestUnsafeLocationNoCase(t *testing.T) {
	s := newSandbox(t)
	refs := Refs(s.home, filepath.Join(s.root, "self"), s.state, filepath.Join(s.state, "lima"))
	upper := strings.ToUpper(s.home)
	// On a file system that ignores case the uppercased home is the
	// home folder, and refused below; here case matters.
	if !paths.NoCase() {
		if _, bad := UnsafeLocation(upper, refs); bad {
			t.Error("refused where case matters")
		}
	}
	old := paths.NoCase
	paths.NoCase = func() bool { return true }
	t.Cleanup(func() { paths.NoCase = old })
	refs = Refs(s.home, filepath.Join(s.root, "self"), s.state, filepath.Join(s.state, "lima"))
	if why, bad := UnsafeLocation(upper, refs); !bad || !strings.Contains(why, "your home directory") {
		t.Errorf("home in capitals: %q %v", why, bad)
	}
	if why, bad := UnsafeLocation(strings.ToUpper(s.state)+"/x", refs); !bad {
		t.Errorf("inside the state dir in capitals: %q %v", why, bad)
	}
}

// Subdirectories of the home folder containing credentials or application data
// may be explicitly shared as projects; the home folder itself, Cage/agent-vm
// state and git metadata directories remain refused.
func TestCredentialDirectoriesAllowed(t *testing.T) {
	s := newSandbox(t)
	for _, d := range []string{".ssh/keys", ".aws", ".config/gcloud", "Library/Keychains", ".local/share/keyrings", ".mozilla", "AppData/Local/proj"} {
		os.MkdirAll(filepath.Join(s.home, d), 0o755)
	}
	os.MkdirAll(filepath.Join(s.root, "appdata", "app"), 0o755)
	t.Setenv("APPDATA", filepath.Join(s.root, "appdata"))
	t.Setenv("LOCALAPPDATA", filepath.Join(s.home, "AppData", "Local"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(s.home, ".config"))
	refs := s.refs()
	for _, dir := range []string{
		s.home + "/.ssh",
		s.home + "/.ssh/keys",
		s.home + "/.aws",
		s.home + "/.config",
		s.home + "/.config/gcloud",
		s.home + "/Library/Keychains",
		s.home + "/.mozilla",
		s.home + "/.local",
		s.home + "/.local/share/keyrings",
		s.home + "/AppData/Local/proj",
		s.root + "/appdata/app",
	} {
		if why, bad := UnsafeProject(dir, refs); bad {
			t.Errorf("UnsafeProject(%s) = %q, want allowed", dir, why)
		}
	}
	for _, dir := range []string{s.home, s.root, s.state, s.state + "/sub", s.root + "/bare.git", s.root + "/repo/.git"} {
		if _, bad := UnsafeProject(dir, refs); !bad {
			t.Errorf("UnsafeProject(%s) = allowed, want refused", dir)
		}
	}
}
