package host

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestSocketParentPermissions(t *testing.T) {
	for _, tc := range []struct {
		name, sddl string
		usable     bool
	}{
		{"standard root", "O:BAD:P(A;;FA;;;BA)(A;;FA;;;SY)(A;;0x1200ad;;;AU)(A;OICIIO;GA;;;CO)", true},
		{"inherit only full control", "O:SYD:P(A;OICIIO;FA;;;BU)", true},
		{"delete children", "O:BAD:P(A;;0x40;;;AU)", false},
		{"change permissions", "O:BAD:P(A;;WD;;;BU)", false},
		{"change owner", "O:BAD:P(A;;WO;;;BU)", false},
		{"generic full control", "O:BAD:P(A;;GA;;;WD)", false},
		{"untrusted owner", "O:BUD:P(A;;FR;;;WD)", false},
		{"no DACL", "O:BA", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(tc.sddl)
			if err != nil {
				t.Fatal(err)
			}
			if err := checkSocketParentDescriptor(sd); (err == nil) != tc.usable {
				t.Fatalf("usable = %t, want %t (%v)", err == nil, tc.usable, err)
			}
		})
	}
}

func TestPrivateSocketDirSystemFallback(t *testing.T) {
	// Both ordinary candidates are under Temp and therefore ineligible.
	profile := t.TempDir()
	t.Setenv("USERPROFILE", profile)
	want := systemSocketDir(profile)
	if want == "" {
		t.Fatal("missing system drive candidate")
	}
	if _, err := os.Lstat(want); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected an unused test directory: %v", err)
	}
	const room = len(`\proxy.sock`)
	if policy := checkSocketParent(filepath.Dir(want)); policy != nil {
		for _, create := range []bool{false, true} {
			if _, err := PrivateSocketDir(profile, room, create); err == nil || errors.Is(err, errSocketDirNotCreated) {
				t.Fatalf("unsafe root accepted or treated as an absent directory (create=%t): %v", create, err)
			}
		}
		t.Logf("system drive policy disallows a shared socket directory: %v", policy)
		return
	}
	if _, err := PrivateSocketDir(profile, room, false); err == nil {
		t.Fatal("lookup unexpectedly adopted a missing directory")
	}
	dir, err := PrivateSocketDir(profile, room, true)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Skipf("this account cannot create a private directory in the system drive: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if dir != want {
		t.Fatalf("selected %q, want %q", dir, want)
	}
	t.Cleanup(func() {
		if err := os.Remove(dir); err != nil {
			t.Error(err)
		}
	})
	if found, err := PrivateSocketDir(profile, room, false); err != nil || found != dir {
		t.Fatalf("lookup got %q, %v", found, err)
	}
	path := filepath.Join(dir, "proxy.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}

func TestSystemDriveSocketDirOwnership(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "CageRun-test")
	if err := mkdirPrivate(dir); err != nil {
		t.Fatal(err)
	}
	if err := ownedPrivateDir(dir); err != nil {
		t.Fatalf("directory made private rejected: %v", err)
	}
	if err := mkdirPrivate(dir); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second creation: %v", err)
	}

	open := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(open, 0700); err != nil {
		t.Fatal(err)
	}
	sid, err := privateSID()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + sid.String() + ")(A;OICI;FR;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(open, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if err := ownedPrivateDir(open); err == nil {
		t.Fatal("directory readable by everyone accepted")
	}

	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ownedPrivateDir(file); err == nil {
		t.Fatal("file accepted as directory")
	}
}
