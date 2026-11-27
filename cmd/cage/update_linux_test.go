package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/appupdate"
	"golang.org/x/sys/unix"
)

func TestLinuxUpdateHandoffWithClosedStdin(t *testing.T) {
	const key = "CAGE_TEST_UPDATE_EXEC_INPUT"
	if path := os.Getenv(key); path != "" {
		if err := unix.Close(0); err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if file.Fd() != 0 {
			t.Fatalf("expected newly opened fd 0, got %d", file.Fd())
		}
		t.Fatal(execLinuxUpdate(t.Context(), file, []string{"/bin/cat"}))
	}
	path := filepath.Join(t.TempDir(), "input")
	const data = "verified package bytes"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxUpdateHandoffWithClosedStdin$")
	cmd.Env = append(os.Environ(), key+"="+path)
	got, err := cmd.CombinedOutput()
	if err != nil || string(got) != data {
		t.Fatalf("handoff returned %v with %q", err, got)
	}
}

func TestLinuxUpdateRejectsUnpackagedExecutables(t *testing.T) {
	for path, want := range map[string]string{
		"/tmp/cage":     "源码构建",
		"/usr/bin/cage": "原安装渠道",
		"/home/test/profiles/cloud-host/bin/cage": "云端部署",
		"/home/test/.local/lib/cage/cage":         "云端部署",
	} {
		if err := checkLinuxInstallation(t.Context(), path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %s", path, err, want)
		}
	}
}

func TestLinuxUpdateValidatesDebianPackage(t *testing.T) {
	const release = "1.2.3-rc.4"
	if _, err := os.Stat("/usr/bin/dpkg-deb"); err != nil {
		t.Skip("requires dpkg-deb")
	}
	build := func(t *testing.T, name, version, arch string) appupdate.State {
		t.Helper()
		dir := t.TempDir()
		control := filepath.Join(dir, "pkg", "DEBIAN")
		if err := os.MkdirAll(control, 0755); err != nil {
			t.Fatal(err)
		}
		metadata := fmt.Sprintf("Package: %s\nVersion: %s\nArchitecture: %s\nMaintainer: Cage Test <test@example.org>\nDescription: Cage test package\n", name, version, arch)
		if err := os.WriteFile(filepath.Join(control, "control"), []byte(metadata), 0644); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "update.deb")
		if data, err := exec.Command("/usr/bin/dpkg-deb", "--build", filepath.Dir(control), path).CombinedOutput(); err != nil {
			t.Fatalf("dpkg-deb: %v %s", err, data)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return appupdate.State{LatestVersion: release, TotalBytes: int64(len(data)), SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), ArchivePath: path}
	}
	t.Run("confirmed file survives cache replacement", func(t *testing.T) {
		pkg := build(t, "claude-cage", "1.2.3~rc.4", "amd64")
		file, err := openLinuxUpdate(t.Context(), pkg, release)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if err := os.Rename(pkg.ArchivePath, pkg.ArchivePath+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pkg.ArchivePath, []byte("next download"), 0600); err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(file)
		if err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != pkg.SHA256 {
			t.Fatal("installer did not receive the confirmed archive")
		}
	})
	for _, tc := range []struct{ name, pkg, version, arch string }{
		{"name", "other-cage", "1.2.3~rc.4", "amd64"},
		{"version", "claude-cage", "1.2.3", "amd64"},
		{"architecture", "claude-cage", "1.2.3~rc.4", "arm64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pkg := build(t, tc.pkg, tc.version, tc.arch)
			if file, err := openLinuxUpdate(t.Context(), pkg, release); err == nil {
				file.Close()
				t.Fatal("accepted incorrect Debian metadata")
			}
		})
	}
	t.Run("release changed", func(t *testing.T) {
		pkg := build(t, "claude-cage", "1.2.3~rc.4", "amd64")
		if file, err := openLinuxUpdate(t.Context(), pkg, "1.2.3"); err == nil {
			file.Close()
			t.Fatal("accepted a different release")
		}
	})
	t.Run("checksum changed", func(t *testing.T) {
		pkg := build(t, "claude-cage", "1.2.3~rc.4", "amd64")
		pkg.SHA256 = strings.Repeat("0", 64)
		if file, err := openLinuxUpdate(t.Context(), pkg, release); err == nil {
			file.Close()
			t.Fatal("accepted incorrect SHA-256")
		}
	})
}
