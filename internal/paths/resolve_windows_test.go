package paths

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestResolveFallsBackWhenDriverLacksNameQuery(t *testing.T) {
	dir := t.TempDir()
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	old := finalPathName
	t.Cleanup(func() { finalPathName = old })
	for _, code := range []windows.Errno{windows.ERROR_INVALID_FUNCTION, windows.ERROR_NOT_SUPPORTED, windows.ERROR_INVALID_PARAMETER} {
		finalPathName = func(windows.Handle, *uint16, uint32, uint32) (uint32, error) { return 0, code }
		got, err := Resolve(dir)
		if err != nil || got != want {
			t.Errorf("%v: Resolve = %q, %v; want %q", code, got, err, want)
		}
	}
	finalPathName = func(windows.Handle, *uint16, uint32, uint32) (uint32, error) { return 0, windows.ERROR_ACCESS_DENIED }
	var pe *os.PathError
	if _, err := Resolve(dir); !errors.As(err, &pe) || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Errorf("other errors must still fail: %v", err)
	}
}
