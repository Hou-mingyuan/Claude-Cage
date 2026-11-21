// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package hostagent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestResourceProbeProcess(t *testing.T) {
	if mode := os.Getenv("CAGE_RESOURCE_PROBE_TEST"); mode != "" {
		input := bufio.NewReader(os.Stdin)
		for {
			if _, err := input.ReadString('\n'); err != nil {
				os.Exit(0)
			}
			switch mode {
			case "failure":
				fmt.Fprint(os.Stdout, "private-stdout")
				fmt.Fprint(os.Stderr, "getsockname failed: Not a socket\n"+strings.Repeat("x", 1<<20))
				os.Exit(7)
			case "timeout":
				time.Sleep(30 * time.Second)
			case "oversize":
				fmt.Fprint(os.Stdout, strings.Repeat("x", 16385)+"\n\x1e\n")
			case "success":
				fmt.Fprint(os.Stdout, "metrics\n\x1e\n")
			}
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"failure", "timeout", "oversize", "success", "start"} {
		t.Run(mode, func(t *testing.T) {
			var starts atomic.Int32
			session := &resourceProbeSession{command: func(ctx context.Context, _ string) *exec.Cmd {
				starts.Add(1)
				cmd := exec.CommandContext(ctx, exe, "-test.run=^TestResourceProbeProcess$")
				cmd.Env = append(os.Environ(), "CAGE_RESOURCE_PROBE_TEST="+mode)
				cmd.WaitDelay = time.Second
				hideResourceProbe(cmd)
				if mode == "start" {
					cmd.Path = "missing-resource-probe-executable"
				}
				return cmd
			}}
			defer session.close()
			read := func() ([]byte, error) {
				timeout := 5 * time.Second
				if mode == "timeout" {
					timeout = 100 * time.Millisecond
				}
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				defer cancel()
				return session.read(ctx, "probe")
			}
			for round := 0; round < 3; round++ {
				out, err := read()
				switch mode {
				case "success":
					if err != nil || string(out) != "metrics\n" {
						t.Fatalf("valid observation lost: %q %v", out, err)
					}
				case "failure":
					if err == nil || !strings.Contains(err.Error(), "Not a socket") || len(err.Error()) > 4300 || strings.Contains(err.Error(), "private-stdout") {
						t.Fatalf("missing or unbounded execution diagnostic: %v", err)
					}
				case "timeout":
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("timeout cause lost: %v", err)
					}
				case "oversize":
					if err == nil || !strings.Contains(err.Error(), "exceeded") {
						t.Fatalf("oversized stdout accepted: %v", err)
					}
				case "start":
					if err == nil || !strings.Contains(err.Error(), "probe start") {
						t.Fatalf("start failure lost: %v", err)
					}
				}
			}
			// Healthy observations share one session; every failure reopens it.
			want := int32(3)
			if mode == "success" {
				want = 1
			}
			if got := starts.Load(); got != want {
				t.Fatalf("sessions opened: got %d, want %d", got, want)
			}
		})
	}
}

func TestResourceProbeLoopRunsScriptPerRequest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the loop runs in the Linux guest's POSIX shell")
	}
	session := &resourceProbeSession{command: func(ctx context.Context, remote string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-c", remote)
	}}
	defer session.close()
	script := "LC_ALL=C; export LC_ALL\nprintf 'quote %s\\n' \"'it'\"\ncat /missing-probe-file 2>/dev/null\necho n=$((${n:-0}+1))"
	for round := 0; round < 2; round++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err := session.read(ctx, script)
		cancel()
		if err != nil || string(out) != "quote 'it'\nn=1\n" {
			t.Fatalf("round %d: %q %v", round, out, err)
		}
	}
}
