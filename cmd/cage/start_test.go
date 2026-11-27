package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/cli"
)

func TestVMStartDispatch(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run("reset="+strconv.FormatBool(reset), func(t *testing.T) {
			args := []string{"run", "true"}
			if reset {
				args = append([]string{"--reset"}, args...)
			}
			var output bytes.Buffer
			stdio := cli.IO{Stdin: strings.NewReader(""), Stdout: &output, Stderr: &output}
			var events []string
			code := startEnvironment(args, stdio, func(got []string, streams cli.IO) int {
				if streams != stdio || !slices.Equal(got, args) {
					t.Fatalf("unexpected VM boot invocation: %q", got)
				}
				events = append(events, "boot")
				return 0
			}, func() error { events = append(events, "services"); return nil })
			if code != 0 || !slices.Equal(events, []string{"boot", "services"}) || !strings.Contains(output.String(), "services are ready") {
				t.Fatalf("exit=%d events=%v output=%q", code, events, output.String())
			}
		})
	}
}

func TestUpRejectsLegacyResetBeforeOpeningProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile", "config.json")
	t.Setenv("CLAUDE_CAGE_CONFIG", path)
	old := os.Args
	t.Cleanup(func() { os.Args = old })
	os.Args = []string{"cage", "up", "--reset"}
	code, _, message := captureRun(t, run)
	if code != 2 || !strings.Contains(message, "cage start --reset") || !strings.Contains(message, "lost") {
		t.Fatalf("code=%d message=%q", code, message)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("opened profile: %v", err)
	}
}

func TestStartUsageBeforeOpeningProfile(t *testing.T) {
	for _, command := range []string{"start", "up", "login"} {
		for _, args := range [][]string{
			{"--unknown"}, {"unexpected"}, {"--reset", "--unknown"},
			{"--help", "--unknown"}, {"-h", "unexpected"},
			{"--rm"}, {"--scratch"}, {"--readonly"}, {"--tty"},
			{"--unsafe-writable-git"}, {"--unsafe-disable-security-prompts"},
			{"--cpus", "2"}, {"--memory=4"}, {"--ram", "4"}, {"--disk", "24"}, {"--ssh-port", "55022"},
		} {
			t.Run(command+"/"+strings.Join(args, " "), func(t *testing.T) {
				profile := filepath.Join(t.TempDir(), "profile")
				t.Setenv("CLAUDE_CAGE_CONFIG", filepath.Join(profile, "config.json"))
				previous := os.Args
				os.Args = append([]string{"cage", command}, args...)
				t.Cleanup(func() { os.Args = previous })
				code, stdout, stderr := captureRun(t, run)
				if code != 2 || stdout != "" || !strings.Contains(stderr, "Usage: cage "+command) {
					t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
				}
				if _, err := os.Stat(profile); !os.IsNotExist(err) {
					t.Fatalf("invalid arguments opened the profile: %v", err)
				}
			})
		}
	}
}

func TestVMStartFailureStopsBeforeNextStage(t *testing.T) {
	for _, phase := range []string{"boot", "services"} {
		t.Run(phase, func(t *testing.T) {
			var events []string
			var output bytes.Buffer
			code, _, _ := captureRun(t, func() int {
				return startEnvironment([]string{"--reset", "run", "true"}, cli.IO{Stdout: &output, Stderr: &output}, func([]string, cli.IO) int {
					events = append(events, "boot")
					if phase == "boot" {
						return 7
					}
					return 0
				}, func() error { events = append(events, "services"); return errors.New("service startup failed") })
			})
			want := []string{"boot"}
			wantCode := 7
			if phase == "services" {
				want = append(want, "services")
				wantCode = 1
			}
			if code != wantCode || !slices.Equal(events, want) {
				t.Fatalf("exit=%d events=%v", code, events)
			}
		})
	}
}
