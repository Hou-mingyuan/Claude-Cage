package host

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestWindowsSocketDirProblem(t *testing.T) {
	excluded := []string{`C:\Users\dev\AppData\Local`, `C:\Users\dev\AppData\Roaming`, `C:\Users\dev\AppData\Local\Temp`}
	for dir, ok := range map[string]bool{
		`C:\Users\dev\.claude-cage\run`:         true,
		`d:\cage\run`:                           true,
		`C:\Users\鲸落\.claude-cage\run`:          false,
		`\\server\share\run`:                    false,
		`run`:                                   false,
		`C:\Users\dev\AppData\Local\Temp\x`:     false,
		`c:\users\DEV\appdata\local. \cage`:     false,
		`C:\Users\dev\AppDataX\run`:             true,
		`C:\` + strings.Repeat("a", 100):        false,
		`C:\` + strings.Repeat("a", 107-3-48):   true,
		`C:\` + strings.Repeat("a", 107-3-48+1): false,
	} {
		if got := windowsSocketDirProblem(dir, 48, excluded) == nil; got != ok {
			t.Errorf("%q: usable %v, want %v", dir, got, ok)
		}
	}
}

// fakeSocketFS is a set of existing directories, volumes without ACLs and
// 8.3 aliases.
type fakeSocketFS struct {
	existing map[string]bool
	noACLs   []string
	short    map[string]string
	made     []string
	// madePrivate are directories created private from the start; taken
	// names exist but are not real directories; foreign directories are
	// not the current user's own private ones.
	madePrivate []string
	taken       map[string]bool
	foreign     map[string]bool
}

func (f *fakeSocketFS) fs() windowsSocketFS {
	return windowsSocketFS{
		persistentACLs: func(dir string) bool { return !WindowsPathWithin(dir, f.noACLs) },
		exists:         func(dir string) bool { return f.existing[dir] },
		mkdir: func(dir string) error {
			f.made = append(f.made, dir)
			f.existing[dir] = true
			return nil
		},
		shortName: func(dir string) (string, error) {
			if s, ok := f.short[dir]; ok && f.existing[dir] {
				return s, nil
			}
			return "", errors.New("8.3 names disabled")
		},
		mkdirPrivate: func(dir string) error {
			if f.existing[dir] || f.taken[dir] {
				return os.ErrExist
			}
			f.madePrivate = append(f.madePrivate, dir)
			f.existing[dir] = true
			return nil
		},
		ownedPrivate: func(dir string) error {
			if f.foreign[dir] || !f.existing[dir] {
				return errors.New("not the current user's private directory")
			}
			return nil
		},
	}
}

func TestChooseWindowsSocketDir(t *testing.T) {
	excluded := []string{`C:\Users\鲸落\AppData\Local`}
	f := &fakeSocketFS{existing: map[string]bool{}, short: map[string]string{`C:\Users\鲸落\.claude-cage\run`: `C:\Users\9B9E~1\.CLAUD~1\run`}}
	candidates := WindowsSocketDirCandidates(`C:\Users\鲸落\.claude-cage`, `C:\Users\鲸落`, "abc")
	if candidates[0] != `C:\Users\鲸落\.claude-cage\run` || candidates[1] != `C:\Users\鲸落\.cage-run\abc` {
		t.Fatalf("candidates: %q", candidates)
	}
	// A lookup before the core made the directory does not settle on
	// another candidate.
	if _, err := chooseWindowsSocketDir(candidates, 48, excluded, f.fs(), false); !errors.Is(err, errSocketDirNotCreated) {
		t.Fatalf("lookup before creation: %v", err)
	}
	if got, err := chooseWindowsSocketDir(candidates, 48, excluded, f.fs(), true); err != nil || got != `C:\Users\9B9E~1\.CLAUD~1\run` {
		t.Fatalf("non-ASCII profile: %q, %v", got, err)
	}
	if got, err := chooseWindowsSocketDir(candidates, 48, excluded, f.fs(), false); err != nil || got != `C:\Users\9B9E~1\.CLAUD~1\run` {
		t.Fatalf("lookup after creation: %q, %v", got, err)
	}
	// A profile in AppData is never used, not even through an alias.
	appData := WindowsSocketDirCandidates(`C:\Users\鲸落\AppData\Local\cage`, `C:\Users\鲸落`, "abc")
	f.short[`C:\Users\鲸落\AppData\Local\cage\run`] = `C:\Users\9B9E~1\AppData\Local\cage\run`
	f.short[`C:\Users\鲸落\.cage-run\abc`] = `C:\Users\9B9E~1\.cage-run\abc`
	if got, err := chooseWindowsSocketDir(appData, 48, excluded, f.fs(), true); err != nil || got != `C:\Users\9B9E~1\.cage-run\abc` {
		t.Fatalf("AppData profile: %q, %v", got, err)
	}
	delete(f.short, `C:\Users\鲸落\.cage-run\abc`)
	delete(f.short, `C:\Users\鲸落\.claude-cage\run`)
	if _, err := chooseWindowsSocketDir(candidates, 48, excluded, f.fs(), true); !errors.Is(err, errSocketDirUnusable) {
		t.Fatalf("no ASCII spelling must fail clearly: %v", err)
	}
	f.made = nil
	if got, err := chooseWindowsSocketDir([]string{`C:\Users\dev\.claude-cage\run`}, 48, excluded, f.fs(), true); err != nil || got != `C:\Users\dev\.claude-cage\run` || len(f.made) != 0 {
		t.Fatalf("ASCII profile: %q, %v, made %q", got, err, f.made)
	}
}

// A profile on exFAT: the run directory there can be
// created but not made private. Whether the directory already exists, and
// whether the process creates or only looks up, every process must pick the
// same directory, or the window shows the core as stopped and restarts it.
func TestWindowsSocketDirIsTheSameForEveryProcess(t *testing.T) {
	excluded := []string{`C:\Users\dev\AppData\Local`}
	candidates := WindowsSocketDirCandidates(`E:\cage-profile`, `C:\Users\dev`, "abc")
	for _, existing := range []map[string]bool{
		{},
		{`E:\cage-profile\run`: true},
		{`E:\cage-profile\run`: true, `C:\Users\dev\.cage-run\abc`: true},
		{`C:\Users\dev\.cage-run\abc`: true},
	} {
		f := &fakeSocketFS{existing: existing, noACLs: []string{`E:\`}, short: map[string]string{}}
		created, err := chooseWindowsSocketDir(candidates, 48, excluded, f.fs(), true)
		if err != nil || created != `C:\Users\dev\.cage-run\abc` {
			t.Fatalf("existing %v: core chose %q, %v", existing, created, err)
		}
		looked, err := chooseWindowsSocketDir(candidates, 48, excluded, f.fs(), false)
		if err != nil || looked != created {
			t.Fatalf("existing %v: lookup chose %q (%v), core %q", existing, looked, err, created)
		}
	}
	// On NTFS the profile's own directory is used by all, existing or not.
	f := &fakeSocketFS{existing: map[string]bool{`C:\Users\dev\.cage-run\abc`: true}, short: map[string]string{}}
	ntfs := WindowsSocketDirCandidates(`D:\cage-profile`, `C:\Users\dev`, "abc")
	for _, create := range []bool{true, false} {
		if got, err := chooseWindowsSocketDir(ntfs, 48, excluded, f.fs(), create); err != nil || got != `D:\cage-profile\run` {
			t.Fatalf("create=%v: %q, %v", create, got, err)
		}
	}
}

// A user directory with no ASCII spelling, not even through 8.3 aliases
// (#1037): the sockets go to the per-user directory in the root of the
// system drive, and every process finds the same one.
func TestWindowsSocketDirFallsBackToSystemDrive(t *testing.T) {
	excluded := []string{`C:\Users\鲸落\AppData\Local`}
	profile := `C:\Users\鲸落\.claude-cage`
	system := WindowsSystemSocketDir("C:", "S-1-5-21-1-2-3-1001", profile)
	if !strings.HasPrefix(system, `C:\CageRun-`) || windowsSocketDirProblem(system, 48, excluded) != nil {
		t.Fatalf("system drive directory %q unusable", system)
	}
	if system != WindowsSystemSocketDir("C:", "S-1-5-21-1-2-3-1001", profile) ||
		system == WindowsSystemSocketDir("C:", "S-1-5-21-1-2-3-1002", profile) ||
		system == WindowsSystemSocketDir("C:", "S-1-5-21-1-2-3-1001", `C:\Users\鲸落\other`) {
		t.Fatalf("system drive directory %q is not one per user and profile", system)
	}
	candidates := append(WindowsSocketDirCandidates(profile, `C:\Users\鲸落`, "abc"), system)
	// The alias Windows gives still contains the Chinese user name.
	f := &fakeSocketFS{existing: map[string]bool{}, short: map[string]string{
		`C:\Users\鲸落\.claude-cage\run`: `C:\Users\鲸落\CLAUDE~1\run`,
		`C:\Users\鲸落\.cage-run\abc`:    `C:\Users\鲸落\CAGE-R~1\abc`,
	}}
	if _, err := chooseWindowsSocketDir(candidates, 48, excluded, f.fs(), false); !errors.Is(err, errSocketDirNotCreated) {
		t.Fatalf("lookup before creation: %v", err)
	}
	got, err := chooseWindowsSocketDir(candidates, 48, excluded, f.fs(), true)
	if err != nil || got != system {
		t.Fatalf("non-ASCII aliases: %q, %v", got, err)
	}
	if err := openSharedSocketDir(got, f.fs(), true); err != nil || len(f.madePrivate) != 1 || f.madePrivate[0] != system {
		t.Fatalf("creating %q: %v, made private %q", got, err, f.madePrivate)
	}
	looked, err := chooseWindowsSocketDir(candidates, 48, excluded, f.fs(), false)
	if err != nil || looked != system {
		t.Fatalf("lookup after creation: %q, %v", looked, err)
	}
	if err := openSharedSocketDir(looked, f.fs(), false); err != nil || len(f.madePrivate) != 1 {
		t.Fatalf("lookup of %q: %v, made private %q", looked, err, f.madePrivate)
	}
	// Earlier candidates that work keep being used.
	ascii := append(WindowsSocketDirCandidates(`C:\Users\dev\.claude-cage`, `C:\Users\dev`, "abc"), system)
	for _, create := range []bool{true, false} {
		f := &fakeSocketFS{existing: map[string]bool{system: true}, short: map[string]string{}}
		if got, err := chooseWindowsSocketDir(ascii, 48, excluded, f.fs(), create); err != nil || got != `C:\Users\dev\.claude-cage\run` {
			t.Fatalf("ASCII profile, create=%v: %q, %v", create, got, err)
		}
	}
	f = &fakeSocketFS{existing: map[string]bool{}, short: map[string]string{`C:\Users\鲸落\.claude-cage\run`: `C:\Users\9B9E~1\.CLAUD~1\run`}}
	if got, err := chooseWindowsSocketDir(candidates, 48, excluded, f.fs(), true); err != nil || got != `C:\Users\9B9E~1\.CLAUD~1\run` {
		t.Fatalf("ASCII alias: %q, %v", got, err)
	}
}

// Other users may create entries in the root of the system drive, so a
// directory there that is not the current user's own private one is
// refused, never adopted.
func TestSystemDriveSocketDirRefusesForeignDirectory(t *testing.T) {
	system := WindowsSystemSocketDir("C:", "S-1-5-21-1-2-3-1001", `C:\Users\鲸落\.claude-cage`)
	for _, create := range []bool{true, false} {
		f := &fakeSocketFS{existing: map[string]bool{system: true}, foreign: map[string]bool{system: true}}
		if err := openSharedSocketDir(system, f.fs(), create); err == nil || errors.Is(err, errSocketDirNotCreated) {
			t.Fatalf("create=%v: foreign directory accepted: %v", create, err)
		}
		if len(f.madePrivate) != 0 || len(f.made) != 0 {
			t.Fatalf("create=%v: made %q %q", create, f.made, f.madePrivate)
		}
	}
	// A name held by something other than a real directory, such as a
	// junction, is not taken for missing.
	f := &fakeSocketFS{existing: map[string]bool{}, taken: map[string]bool{system: true}, foreign: map[string]bool{system: true}}
	if err := openSharedSocketDir(system, f.fs(), true); err == nil || errors.Is(err, errSocketDirNotCreated) {
		t.Fatalf("junction accepted: %v", err)
	}
}
