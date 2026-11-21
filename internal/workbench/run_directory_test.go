package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/cage"
)

type fakeRunShares struct {
	state   runProjects
	err     error
	opened  []string
	saved   []string
	saveErr error
	openErr error
}

func (f *fakeRunShares) projects(context.Context) (runProjects, error) { return f.state, f.err }
func (f *fakeRunShares) open(_ context.Context, workspace string) error {
	f.opened = append(f.opened, workspace)
	return f.openErr
}
func (f *fakeRunShares) save(_ context.Context, folder string) error {
	f.saved = append(f.saved, folder)
	if f.saveErr == nil {
		f.state.Projects = append(f.state.Projects, folder)
	}
	return f.saveErr
}

func runDirectoryFixture(t *testing.T) (profile, base string) {
	t.Helper()
	temp := t.TempDir()
	profile, base = filepath.Join(temp, "profile"), filepath.Join(temp, "host")
	for _, dir := range []string{profile, filepath.Join(base, "demo", "src", "pkg"), filepath.Join(base, "demo-two"), filepath.Join(base, "docs"), filepath.Join(base, "loose")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if real, err := filepath.EvalSymlinks(temp); err == nil {
		temp = real
		profile, base = filepath.Join(temp, "profile"), filepath.Join(temp, "host")
	}
	for _, p := range []string{profile, base} {
		if !strings.HasPrefix(p, temp+string(filepath.Separator)) {
			t.Fatalf("fixture path %s is outside the test's temporary directory", p)
		}
	}
	return profile, base
}

func TestRunDirectoryFollowsSharedProjectsAndSubdirectories(t *testing.T) {
	profile, base := runDirectoryFixture(t)
	demo, docs := filepath.Join(base, "demo"), filepath.Join(base, "docs")
	shares := &fakeRunShares{state: runProjects{Ready: true, Projects: []string{demo, "/cage-client/abc"},
		Groups: []projectGroup{{Workspace: demo, Folders: []string{demo, docs}}}}}
	ctx := context.Background()
	never := func(string) (bool, error) { t.Fatal("asked to share an already shared folder"); return false, nil }

	got, err := runDirectory(ctx, profile, demo, shares, never)
	if err != nil || got != guestPath(demo) {
		t.Fatalf("project root = %q, %v", got, err)
	}
	got, err = runDirectory(ctx, profile, filepath.Join(demo, "src", "pkg"), shares, never)
	if err != nil || got != guestPath(demo)+"/src/pkg" {
		t.Fatalf("subdirectory = %q, %v", got, err)
	}
	// A second folder of a project connects through that project.
	got, err = runDirectory(ctx, profile, docs, shares, never)
	if err != nil || got != guestPath(docs) || shares.opened[len(shares.opened)-1] != demo {
		t.Fatalf("group folder = %q, %v, opened %v", got, err, shares.opened)
	}
	// A symlink into a project is followed to the project.
	link := filepath.Join(base, "loose", "link")
	if err := os.Symlink(filepath.Join(demo, "src"), link); err != nil {
		// Windows needs a privilege or developer mode for symlinks.
		if runtime.GOOS != "windows" {
			t.Fatal(err)
		}
	} else if got, err = runDirectory(ctx, profile, link, shares, never); err != nil || got != guestPath(demo)+"/src" {
		t.Fatalf("symlinked directory = %q, %v", got, err)
	}
	if len(shares.saved) != 0 {
		t.Fatalf("shared folders saved again: %v", shares.saved)
	}
	// The environment's own directory needs neither the service nor a share.
	env := cage.EnvironmentDir(profile)
	if !strings.HasPrefix(env, filepath.Dir(profile)+string(filepath.Separator)) {
		t.Fatalf("environment directory %s is outside the test's temporary directory", env)
	}
	if err := os.MkdirAll(env, 0700); err != nil {
		t.Fatal(err)
	}
	offline := &fakeRunShares{err: errors.New("offline")}
	if got, err := runDirectory(ctx, profile, env, offline, nil); err != nil || got != guestPath(env) {
		t.Fatalf("environment directory = %q, %v", got, err)
	}
}

func TestRunDirectoryNeverRunsElsewhereWhenNotShared(t *testing.T) {
	profile, base := runDirectoryFixture(t)
	demo, sibling := filepath.Join(base, "demo"), filepath.Join(base, "demo-two")
	ctx := context.Background()
	state := runProjects{Ready: true, Projects: []string{demo}}

	// "demo-two" shares a prefix with "demo" but is not inside it.
	shares := &fakeRunShares{state: state}
	if _, err := runDirectory(ctx, profile, sibling, shares, nil); err == nil || !strings.Contains(err.Error(), "尚未共享") || !strings.Contains(err.Error(), sibling) {
		t.Fatalf("non-interactive unshared run: %v", err)
	}
	if len(shares.saved) != 0 || len(shares.opened) != 0 {
		t.Fatalf("non-interactive run changed shares: %+v", shares)
	}

	asked := ""
	if _, err := runDirectory(ctx, profile, sibling, shares, func(folder string) (bool, error) { asked = folder; return false, nil }); err == nil || !strings.Contains(err.Error(), "已取消") {
		t.Fatalf("declined share: %v", err)
	}
	if asked != sibling || len(shares.saved) != 0 {
		t.Fatalf("declined share asked %q, saved %v", asked, shares.saved)
	}

	if _, err := runDirectory(ctx, profile, sibling, shares, func(string) (bool, error) { return false, errors.New("closed") }); err == nil || len(shares.saved) != 0 {
		t.Fatalf("unanswered prompt: %v, saved %v", err, shares.saved)
	}

	failing := &fakeRunShares{state: state, saveErr: errors.New("请选择具体项目")}
	if _, err := runDirectory(ctx, profile, sibling, failing, func(string) (bool, error) { return true, nil }); err == nil || !strings.Contains(err.Error(), "请选择具体项目") {
		t.Fatalf("refused share: %v", err)
	}

	got, err := runDirectory(ctx, profile, sibling, shares, func(string) (bool, error) { return true, nil })
	if err != nil || got != guestPath(sibling) || len(shares.saved) != 1 || shares.saved[0] != sibling {
		t.Fatalf("confirmed share = %q, %v, saved %v", got, err, shares.saved)
	}

	if _, err := runDirectory(ctx, profile, demo, &fakeRunShares{err: errors.New("offline")}, nil); err == nil || !strings.Contains(err.Error(), "--workdir") {
		t.Fatalf("service unavailable: %v", err)
	}
	if _, err := runDirectory(ctx, profile, demo, &fakeRunShares{state: runProjects{Projects: []string{demo}}}, nil); err == nil || !strings.Contains(err.Error(), "尚未就绪") {
		t.Fatalf("environment not ready: %v", err)
	}
}

func TestGuestRemainderKeepsHostSpelling(t *testing.T) {
	root := filepath.Join(string(filepath.Separator)+"work", "Demo")
	for dir, want := range map[string]string{
		root:                                 "",
		filepath.Join(root, "Src", "My Pkg"): "/Src/My Pkg",
	} {
		if got := guestRemainder(root, dir); got != want {
			t.Errorf("guestRemainder(%q) = %q, want %q", dir, got, want)
		}
	}
}

func TestRunDirectoryReadsTheStatusTheWorkbenchServes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	s := testServer(t)
	s.ready = true
	if !strings.HasPrefix(s.profile, filepath.Clean(os.TempDir())) && !strings.HasPrefix(s.profile, home) {
		t.Fatalf("test profile %s is not temporary", s.profile)
	}
	project := filepath.Join(t.TempDir(), "demo")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(filepath.Join(s.profile, "projects.json"), sharedProjects{Paths: []string{project}}); err != nil {
		t.Fatal(err)
	}
	response := desktopRequestForTest(s, "GET", "/api/status", nil)
	var state runProjects
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &state) != nil {
		t.Fatalf("status: %d %s", response.Code, response.Body.String())
	}
	if !state.Ready || !slices.Contains(state.Projects, project) {
		t.Fatalf("status as read by cage run: %+v", state)
	}
}

// cage run falls back to the environment directory on each of these; it
// needs a short reason and whether sharing the folder would help.
func TestRunDirectoryFailuresCarryAShortReason(t *testing.T) {
	profile, base := runDirectoryFixture(t)
	demo, sibling := filepath.Join(base, "demo"), filepath.Join(base, "demo-two")
	ctx := context.Background()
	ready := runProjects{Ready: true, Projects: []string{demo}}
	for _, tc := range []struct {
		name     string
		dir      string
		shares   *fakeRunShares
		confirm  RunDirectoryConfirm
		unshared bool
		reason   string
	}{
		{"unshared", sibling, &fakeRunShares{state: ready}, nil, true, "未共享"},
		{"declined", sibling, &fakeRunShares{state: ready}, func(string) (bool, error) { return false, nil }, true, "取消"},
		{"share refused", sibling, &fakeRunShares{state: ready, saveErr: errors.New("请选择具体项目\n详情")}, func(string) (bool, error) { return true, nil }, true, "请选择具体项目"},
		{"service unavailable", demo, &fakeRunShares{err: errors.New("dial unix: connection refused")}, nil, false, "connection refused"},
		{"not ready", demo, &fakeRunShares{state: runProjects{Projects: []string{demo}}}, nil, false, "尚未就绪"},
		{"project busy", demo, &fakeRunShares{state: ready, openErr: errors.New("另一个项目操作正在进行")}, nil, false, "另一个项目操作正在进行"},
		{"unreadable", filepath.Join(base, "missing"), &fakeRunShares{state: ready}, nil, false, "无法读取当前目录"},
	} {
		_, err := runDirectory(ctx, profile, tc.dir, tc.shares, tc.confirm)
		var failure *RunDirectoryError
		if !errors.As(err, &failure) {
			t.Fatalf("%s: %T %v is not a RunDirectoryError", tc.name, err, err)
		}
		if failure.Unshared != tc.unshared || !strings.Contains(failure.Reason, tc.reason) || strings.Contains(failure.Reason, "\n") {
			t.Fatalf("%s: reason %q unshared %v", tc.name, failure.Reason, failure.Unshared)
		}
	}
}
