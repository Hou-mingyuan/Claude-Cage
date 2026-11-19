package mounts

import (
	"bytes"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/paths"
)

// A repository's own folder is found up to the drive, the drive's root
// excepted, as "/" is elsewhere.
func windowsFixture(t *testing.T) string {
	t.Helper()
	old := paths.Windows
	paths.Windows = func() bool { return true }
	t.Cleanup(func() { paths.Windows = old })
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		return paths.Host(dir) + "/"
	}
	t.Chdir(dir)
	return "C:/"
}

func TestWindowsGitDirShare(t *testing.T) {
	root := windowsFixture(t)
	mkdir := func(s string) {
		t.Helper()
		if err := os.MkdirAll(root+s, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Only simulate a bare repository at the drive root on POSIX.
	if runtime.GOOS != "windows" {
		mkdir("objects")
		mkdir("refs")
		if err := os.WriteFile(root+"HEAD", nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"proj", "bare/objects", "bare/refs", "bare/sub"} {
		mkdir(d)
	}
	if err := os.WriteFile(root+"bare/HEAD", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if why, bad := GitDirShare(root + "proj"); bad {
		t.Errorf("the root taken for a repository: %s", why)
	}
	bare, err := paths.Real(root + "bare")
	if err != nil {
		t.Fatal(err)
	}
	if why, bad := GitDirShare(root + "bare/sub"); !bad || !strings.Contains(why, "("+bare+")") {
		t.Errorf("inside a bare repository: %q %v", why, bad)
	}
}

// The volumes file and the shares on Windows, on any host.
func TestWindowsVolumes(t *testing.T) {
	root := windowsFixture(t)
	host := func(p string) string { return root + p }
	guest := func(p string) string { return paths.Guest(host(p)) }
	for _, d := range []string{"Users/me/proj", "data", "data2", "data3", "data4", "data5", "data6", "st"} {
		if err := os.MkdirAll(host(d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(host("f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := hideDrives(`C:/data:/mnt/d:rw:C:/work/*`); got != "C"+driveColon+"/data:/mnt/d:rw:C"+driveColon+"/work/*" {
		t.Errorf("hideDrives: %q", got)
	}
	if got := hideDrives(`C:\data:x`); got != "C"+driveColon+`\data:x` {
		t.Errorf(`hideDrives(C:\data): %q`, got)
	}

	vols := strings.Join([]string{
		host("data"), // its own path in the VM
		strings.ReplaceAll(host("data2"), "/", `\`) + ":/mnt/d2:rw",
		guest("data3"), // Git Bash spelling
		host("data4") + ":sub:ro:" + host("Users/me/*"),
		host("data5") + ":/mnt/x:ro:" + host("other/*"), // not matched
		host("data6") + ":/mnt/d6:ro:" + guest("Users/me/proj"),
		host("data6") + ":C:/x", // destination with a drive: refused
		host("f.txt"),
		host("Users"), // would cover the project
	}, "\n") + "\n"
	if err := os.WriteFile(host("vols"), []byte(vols), 0o644); err != nil {
		t.Fatal(err)
	}
	var warn bytes.Buffer
	es := Entries(host("vols"), host("Users/me/proj"), host("Users/me"), nil, &warn)
	var got []string
	for _, e := range es {
		got = append(got, e.Src+">"+e.Dst+">"+e.Mode)
	}
	want := strings.Join([]string{host("data") + ">>ro", host("data2") + ">/mnt/d2>rw", host("data3") + ">>ro",
		host("data4") + ">sub>ro", host("data6") + ">/mnt/d6>ro", host("f.txt") + ">>ro"}, " ")
	if strings.Join(got, " ") != want {
		t.Errorf("entries:\n got  %s\n want %s\n%s", strings.Join(got, " "), want, warn.String())
	}
	if !strings.Contains(warn.String(), "'"+host("data6")+":C:/x'") ||
		!strings.Contains(warn.String(), "'"+host("Users")+"' (from ~/.agent-vm/volumes) would be mounted at "+guest("Users")+", which covers the project") ||
		strings.Count(warn.String(), "Warning") != 2 {
		t.Errorf("warnings: %s", warn.String())
	}

	ms, files := Build(Shares{VM: "v", Dir: host("Users/me/proj"), Writable: true, Entries: es, StateDir: host("st"), Warn: &warn})
	if len(ms) == 0 {
		t.Fatal("no mounts")
	}
	var mounts []string
	for _, m := range ms[:len(ms)-1] {
		mounts = append(mounts, m.Location+">"+m.MountPoint)
	}
	want = strings.Join([]string{host("Users/me/proj") + ">" + guest("Users/me/proj"), host("data") + ">" + guest("data"),
		host("data2") + ">/mnt/d2", host("data3") + ">" + guest("data3"), host("data4") + ">" + guest("Users/me/proj/sub"),
		host("data6") + ">/mnt/d6"}, " ")
	if strings.Join(mounts, " ") != want {
		t.Errorf("mounts:\n got  %s\n want %s", strings.Join(mounts, " "), want)
	}
	if len(files) != 1 || files[0].BindDst != guest("f.txt") || !strings.HasPrefix(files[0].Staging, host("st/file-mounts/v/")) || ms[len(ms)-1].MountPoint != "/tmp/.agent-vm-file-mounts/0" {
		t.Errorf("single file: %+v %+v", files, ms[len(ms)-1])
	}
}
