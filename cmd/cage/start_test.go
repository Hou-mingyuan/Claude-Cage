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

func TestStartEnvironmentDispatch(t *testing.T) {
	for _, command := range []string{"up", "login"} {
		for _, reset := range []bool{false, true} {
			t.Run(command+"/reset="+strconv.FormatBool(reset), func(t *testing.T) {
				args := []string{"run", "true"}
				if reset {
					args = append([]string{"--reset"}, args...)
				}
				input := strings.NewReader("login input")
				var stdout, stderr bytes.Buffer
				stdio := cli.IO{Stdin: input, Stdout: &stdout, Stderr: &stderr}
				var events []string
				invoke := func(got []string, streams cli.IO) int {
					if streams != stdio {
						t.Fatal("start or login replaced interactive IO")
					}
					if len(events) == 0 {
						if !slices.Equal(got, args) {
							t.Errorf("start got %q, want %q", got, args)
						}
						events = append(events, "start")
					} else {
						want := []string{"run", "--tty", "bash", "-lc", "source /etc/profile.d/claude-cage.sh; exec claude auth login"}
						if !slices.Equal(got, want) {
							t.Errorf("login got %q, want %q", got, want)
						}
						events = append(events, "login")
					}
					return 0
				}
				code := startEnvironment(command, args, stdio, invoke, func() error {
					events = append(events, "services")
					return nil
				})
				want := []string{"start", "services"}
				if command == "login" {
					want = append(want, "login")
				}
				if code != 0 || !slices.Equal(events, want) || !strings.Contains(stdout.String(), "services are ready") {
					t.Fatalf("exit=%d events=%q stdout=%q stderr=%q", code, events, stdout.String(), stderr.String())
				}
			})
		}
	}
}

func TestStartUsageBeforeOpeningProfile(t *testing.T) {
	for _, command := range []string{"up", "login"} {
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

func TestStartEnvironmentStopsOnFailure(t *testing.T) {
	for _, phase := range []string{"start", "services", "login"} {
		t.Run(phase, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			var events []string
			invoke := func(args []string, stdio cli.IO) int {
				event := "start"
				if slices.Contains(args, "--tty") {
					event = "login"
				}
				events = append(events, event)
				if event == phase {
					return 7
				}
				return 0
			}
			code, _, _ := captureRun(t, func() int {
				return startEnvironment("login", []string{"--reset", "run", "true"}, cli.IO{Stdout: &stdout, Stderr: &stderr}, invoke, func() error {
					events = append(events, "services")
					if phase == "services" {
						return errors.New("service startup failed")
					}
					return nil
				})
			})
			want, wantCode := []string{"start"}, 7
			if phase != "start" {
				want = append(want, "services")
			}
			if phase == "services" {
				wantCode = 1
			} else if phase == "login" {
				want = append(want, "login")
			}
			if code != wantCode || !slices.Equal(events, want) {
				t.Fatalf("exit=%d events=%q, want exit=%d events=%q", code, events, wantCode, want)
			}
		})
	}
}
