package cli

import (
	"slices"
	"testing"
)

func TestTakeWorkdirOnlyReadsRunOptions(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		workdir string
		rest    []string
		bad     bool
	}{
		{[]string{"--tty", "--workdir", "/p", "claude"}, "/p", []string{"--tty", "claude"}, false},
		{[]string{"--rm", "--workdir=/p q", "--", "ls", "--workdir", "x"}, "/p q", []string{"--rm", "--", "ls", "--workdir", "x"}, false},
		{[]string{"--memory", "4", "pwd", "--workdir=/x"}, "", []string{"--memory", "4", "pwd", "--workdir=/x"}, false},
		{[]string{"--workdir", "relative", "pwd"}, "", nil, true},
		{[]string{"--workdir"}, "", nil, true},
		{[]string{"--workdir=/a", "--workdir=/b", "pwd"}, "", nil, true},
	} {
		workdir, rest, err := TakeWorkdir(tc.args)
		if (err != nil) != tc.bad || workdir != tc.workdir || (!tc.bad && !slices.Equal(rest, tc.rest)) {
			t.Errorf("TakeWorkdir(%q) = %q, %q, %v", tc.args, workdir, rest, err)
		}
	}
	workdir, tty, vmOptions, command, err := SplitRun([]string{"--tty", "--workdir", "/p", "claude", "-p"})
	if err != nil || workdir != "/p" || !tty || vmOptions || !slices.Equal(command, []string{"claude", "-p"}) {
		t.Fatalf("SplitRun = %q %v %v %q %v", workdir, tty, vmOptions, command, err)
	}
	if _, _, vmOptions, _, _ := SplitRun([]string{"--rm", "pwd"}); !vmOptions {
		t.Fatal("SplitRun missed a VM option")
	}
}

func TestTakeRunFlagOnlyReadsRunOptions(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		given bool
		rest  []string
	}{
		{[]string{"--here", "pwd"}, true, []string{"pwd"}},
		{[]string{"--tty", "--workdir", "/p", "--here", "claude"}, true, []string{"--tty", "--workdir", "/p", "claude"}},
		{[]string{"--rm", "--here", "--", "ls", "--here"}, true, []string{"--rm", "--", "ls", "--here"}},
		{[]string{"pwd", "--here"}, false, []string{"pwd", "--here"}},
		{[]string{"--workdir=/x", "ls"}, false, []string{"--workdir=/x", "ls"}},
	} {
		given, rest, err := TakeRunFlag(tc.args, "--here")
		if err != nil || given != tc.given || !slices.Equal(rest, tc.rest) {
			t.Errorf("TakeRunFlag(%q) = %v, %q, %v", tc.args, given, rest, err)
		}
	}
}
