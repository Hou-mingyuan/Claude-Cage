package host

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

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
