//go:build !windows

package workbench

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A plugin application's session records its display and authority where a
// Desktop instance's does, and has no settings manager to follow a density.
func TestDesktopAppResizeScriptReachesTheAppDisplay(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if runtime.GOOS != "linux" || err != nil {
		t.Skip("the script runs inside the Linux environment")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(filepath.Dir(executable), "cage-app-resize-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	id := desktopAppInstance("workshop", "repo-app")
	run, bin := filepath.Join(root, "run"), filepath.Join(root, "bin")
	for _, directory := range []string{run, bin} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, content string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string) string {
		data, _ := os.ReadFile(path)
		return string(data)
	}
	t.Setenv("CAGE_RESIZE_FIXTURE", root)
	write(filepath.Join(bin, "systemctl"), "#!/bin/sh\necho \"$*\" >> \"$CAGE_RESIZE_FIXTURE/units\"\ncat \"$CAGE_RESIZE_FIXTURE/main-pid\"\n", 0o700)
	write(filepath.Join(bin, "sudo"), "#!/bin/sh\necho \"$*\" >> \"$CAGE_RESIZE_FIXTURE/sudo\"\nwhile [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n", 0o700)
	write(filepath.Join(bin, "xrandr"), "#!/bin/sh\necho \"$DISPLAY $XAUTHORITY $*\" >> \"$CAGE_RESIZE_FIXTURE/modes\"\n", 0o700)
	write(filepath.Join(root, "main-pid"), "4242\n", 0o600)
	// As Xtigervnc -displayfd and linux-app-desktop.sh leave them.
	write(filepath.Join(run, "display"), "5\n", 0o600)
	write(filepath.Join(run, "xsettings.conf"), "Gtk/DecorationLayout \":\"\n", 0o600)
	resize := func(scale string) (string, error) {
		t.Helper()
		script := strings.ReplaceAll(accountDesktopResizeScript(id, 1000, 800, scale), "/run/desktop-"+id, run)
		cmd := exec.Command(bash, "--noprofile", "--norc", "-c", script)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		output, err := cmd.CombinedOutput()
		return string(output), err
	}

	if output, err := resize("2"); err != nil || output != "" {
		t.Fatalf("a fixed-density application reported a density: %q %v", output, err)
	}
	if conf := read(filepath.Join(run, "xsettings.conf")); conf != "Gtk/DecorationLayout \":\"\n" {
		t.Fatalf("settings changed: %q", conf)
	}
	if units := read(filepath.Join(root, "units")); !strings.Contains(units, "desktop-"+id+".service") {
		t.Fatalf("not the application's unit: %q", units)
	}
	if modes := read(filepath.Join(root, "modes")); !strings.Contains(modes, ":5 "+run+"/Xauthority --output VNC-0 --mode cage-1000x800") {
		t.Fatalf("size not applied on the application's display: %q", modes)
	}
	if entered := read(filepath.Join(root, "sudo")); !strings.HasPrefix(entered, "-n nsenter --target 4242 ") {
		t.Fatalf("mode commands did not run beside the application: %q", entered)
	}

	write(filepath.Join(root, "main-pid"), "0\n", 0o600)
	modes := read(filepath.Join(root, "modes"))
	if _, err := resize(""); err == nil || read(filepath.Join(root, "modes")) != modes {
		t.Fatal("a closed application was resized")
	}
}
