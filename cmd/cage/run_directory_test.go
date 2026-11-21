package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/workbench"
)

func TestRunFollowsCallerDirectoryUnlessWorkdirGiven(t *testing.T) {
	original := runDirectory
	t.Cleanup(func() { runDirectory = original })
	calls := 0
	runDirectory = func(_ context.Context, configPath, hostDir string, confirm workbench.RunDirectoryConfirm) (string, error) {
		calls++
		if configPath != "/profile/config.json" || hostDir != "/host/demo/src" || confirm != nil {
			t.Fatalf("RunDirectory(%q, %q, confirm=%v)", configPath, hostDir, confirm != nil)
		}
		return "/host/demo/src", nil
	}
	var stderr strings.Builder
	got, err := followRunDirectory("/profile/config.json", []string{"run", "--tty", "claude"}, "/host/demo/src", nil, &stderr, nil)
	if err != nil || !slices.Equal(got, []string{"run", "--workdir", "/host/demo/src", "--tty", "claude"}) || stderr.Len() != 0 {
		t.Fatalf("followed run = %q, %v, %q", got, err, stderr.String())
	}
	for _, args := range [][]string{
		{"run", "--workdir", "/srv/elsewhere", "pwd"},
		{"run", "--tty", "--workdir=/srv/elsewhere", "pwd"},
		{"run"},
		{"run", "--tty"},
	} {
		got, err := followRunDirectory("/profile/config.json", args, "/host/demo/src", nil, &stderr, nil)
		if err != nil || !slices.Equal(got, args) {
			t.Fatalf("%q became %q, %v", args, got, err)
		}
	}
	if calls != 1 {
		t.Fatalf("RunDirectory called %d times", calls)
	}
}

// Before #325 every cage run ran from the environment directory. Scripts,
// cron jobs and editor integrations call it from anywhere: whenever the
// current directory cannot be used, it must still run there.
func TestRunFallsBackToTheEnvironmentDirectory(t *testing.T) {
	original := runDirectory
	t.Cleanup(func() { runDirectory = original })
	asked := func(string) (bool, error) { t.Fatal("a fallback run must not ask to share"); return false, nil }
	for _, tc := range []struct {
		name    string
		err     error
		hostErr error
		confirm workbench.RunDirectoryConfirm
		notice  string
		hint    bool
	}{
		{name: "unshared script", err: &workbench.RunDirectoryError{Reason: "当前目录未共享给 Cage", Unshared: true}, notice: "未共享"},
		{name: "unshared terminal", err: &workbench.RunDirectoryError{Reason: "当前目录未共享给 Cage", Unshared: true}, confirm: asked, notice: "未共享", hint: true},
		{name: "control service down", err: &workbench.RunDirectoryError{Reason: "无法确认当前目录是否已共享"}, confirm: asked, notice: "无法确认"},
		{name: "not ready", err: &workbench.RunDirectoryError{Reason: "Cage 环境尚未就绪"}, notice: "尚未就绪"},
		{name: "project busy (409)", err: &workbench.RunDirectoryError{Reason: "无法连接当前目录所在的项目：busy"}, notice: "busy"},
		{name: "unexpected error", err: errors.New("context deadline exceeded\nmore"), notice: "deadline exceeded"},
		{name: "cwd unreadable", hostErr: errors.New("getwd: no such file"), notice: "无法读取当前目录"},
	} {
		called := false
		runDirectory = func(_ context.Context, _, _ string, confirm workbench.RunDirectoryConfirm) (string, error) {
			called = true
			if confirm != nil {
				t.Fatalf("%s: fallback mode passed a confirm", tc.name)
			}
			return "", tc.err
		}
		var stderr strings.Builder
		args := []string{"run", "--tty", "make", "test"}
		got, err := followRunDirectory("/profile/config.json", args, "/host/x", tc.hostErr, &stderr, tc.confirm)
		if err != nil || !slices.Equal(got, args) {
			t.Fatalf("%s: run = %q, %v; want unchanged args", tc.name, got, err)
		}
		lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
		if !strings.Contains(lines[0], tc.notice) || !strings.Contains(lines[0], "环境目录") {
			t.Fatalf("%s: notice %q", tc.name, stderr.String())
		}
		if hint := len(lines) == 2 && strings.Contains(lines[1], "--here"); hint != tc.hint || len(lines) > 2 {
			t.Fatalf("%s: hint shown %v, stderr %q", tc.name, hint, stderr.String())
		}
		if called == (tc.hostErr != nil) {
			t.Fatalf("%s: RunDirectory called = %v", tc.name, called)
		}
	}
}

func TestRunHereIsStrict(t *testing.T) {
	original := runDirectory
	t.Cleanup(func() { runDirectory = original })
	confirm := func(string) (bool, error) { return true, nil }
	runDirectory = func(_ context.Context, _, _ string, got workbench.RunDirectoryConfirm) (string, error) {
		if got == nil {
			t.Fatal("--here must offer to share on a terminal")
		}
		return "/host/x", nil
	}
	var stderr strings.Builder
	got, err := followRunDirectory("/p/config.json", []string{"run", "--here", "--tty", "pwd"}, "/host/x", nil, &stderr, confirm)
	if err != nil || !slices.Equal(got, []string{"run", "--workdir", "/host/x", "--tty", "pwd"}) {
		t.Fatalf("--here run = %q, %v", got, err)
	}
	runDirectory = func(context.Context, string, string, workbench.RunDirectoryConfirm) (string, error) {
		return "", &workbench.RunDirectoryError{Reason: "当前目录未共享给 Cage", Unshared: true}
	}
	if got, err := followRunDirectory("/p/config.json", []string{"run", "--here", "pwd"}, "/host/x", nil, &stderr, nil); err == nil || got != nil {
		t.Fatalf("--here unshared = %q, %v", got, err)
	}
	if got, err := followRunDirectory("/p/config.json", []string{"run", "--here", "pwd"}, "", errors.New("gone"), &stderr, nil); err == nil || got != nil {
		t.Fatalf("--here without a current directory = %q, %v", got, err)
	}
	if _, err := followRunDirectory("/p/config.json", []string{"run", "--here", "--workdir", "/x", "pwd"}, "/host/x", nil, &stderr, nil); err == nil {
		t.Fatal("--here with --workdir was accepted")
	}
	// The command's own --here is not the run's.
	runDirectory = func(context.Context, string, string, workbench.RunDirectoryConfirm) (string, error) {
		return "/host/x", nil
	}
	if got, _ := followRunDirectory("/p/config.json", []string{"run", "grep", "--here"}, "/host/x", nil, &stderr, nil); !slices.Equal(got, []string{"run", "--workdir", "/host/x", "grep", "--here"}) {
		t.Fatalf("command flag taken: %q", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("strict runs printed a fallback notice: %q", stderr.String())
	}
}

func TestAskShareReadsOnlyTheAnswer(t *testing.T) {
	for answer, want := range map[string]bool{"y\nrest": true, "YES\n": true, "是\n": true, "\n": false, "n\n": false, "": false, "yes please\n": false} {
		in := strings.NewReader(answer)
		var out strings.Builder
		got, err := askShare(in, &out, "/host/demo")
		if err != nil || got != want || !strings.Contains(out.String(), "/host/demo") {
			t.Fatalf("answer %q = %v, %v", answer, got, err)
		}
		if answer == "y\nrest" {
			if rest := answer[len(answer)-in.Len():]; rest != "rest" {
				t.Fatalf("prompt consumed the command's input: left %q", rest)
			}
		}
	}
}
