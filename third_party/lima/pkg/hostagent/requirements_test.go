// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package hostagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/lima-vm/sshocker/pkg/ssh"

	"github.com/lima-vm/lima/v2/pkg/limatype"
)

func TestFinalRequirementRetryBudget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX test ssh executable")
	}
	dir := t.TempDir()
	probes := filepath.Join(dir, "probes")
	marker := filepath.Join(dir, "lima-boot-done")
	t.Setenv("BOOT_PROBES", probes)
	t.Setenv("BOOT_MARKER", marker)
	t.Setenv("BLOCK_BOOT_PROBE", "")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// The guest reports the marker on its 201st probe, just beyond the
	// normal retry budget. Run the actual guest check against a private marker.
	const script = `#!/bin/sh
program=$(sed 's|/var/run/lima-boot-done|$BOOT_MARKER|g;s|/run/lima-boot-done|$BOOT_MARKER|g')
if [ "$BLOCK_BOOT_PROBE" = 1 ]; then
    echo started > "$BOOT_PROBES"
    exec sleep 10
fi
read -r count < "$BOOT_PROBES"
count=$((count+1))
echo "$count" > "$BOOT_PROBES"
if [ "$count" -eq 201 ]; then echo current-boot > "$BOOT_MARKER"; fi
exec sh -c "$program"
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	osType := limatype.LINUX
	a := &HostAgent{instConfig: &limatype.LimaYAML{OS: &osType}, sshConfig: &ssh.SSHConfig{}, iid: "current-boot"}
	for _, tc := range []struct {
		name     string
		deadline time.Duration
		want     string
	}{
		{"ordinary", 0, "200"},
		{"monitored", cloudInitMonitorTimeout, "201"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(marker, []byte("previous-boot\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(probes, []byte("0\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			synctest.Test(t, func(t *testing.T) {
				var deadline time.Time
				if tc.deadline != 0 {
					deadline = time.Now().Add(tc.deadline)
				}
				err := a.waitForFinalRequirements(t.Context(), deadline)
				if (err == nil) != (tc.deadline != 0) {
					t.Fatalf("unexpected readiness result: %v", err)
				}
			})
			got, err := os.ReadFile(probes)
			if err != nil || strings.TrimSpace(string(got)) != tc.want {
				t.Fatalf("probe count %q, want %s: %v", got, tc.want, err)
			}
		})
	}
	t.Run("cancel active SSH", func(t *testing.T) {
		t.Setenv("BLOCK_BOOT_PROBE", "1")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- a.waitForFinalRequirements(ctx, time.Now().Add(time.Minute)) }()
		timeout := time.After(5 * time.Second)
		for {
			if got, _ := os.ReadFile(probes); strings.TrimSpace(string(got)) == "started" {
				break
			}
			select {
			case <-timeout:
				t.Fatal("SSH probe did not start")
			case <-time.After(10 * time.Millisecond):
			}
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("got %v after canceling SSH", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("SSH probe kept running after cancellation")
		}
	})
}

func TestRequirementWaitStopsBetweenProbes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, userCancel := range []bool{false, true} {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			calls := 0
			err := checkRequirements(ctx, "final", []requirement{{description: "boot marker"}}, 0, func(requirement) error {
				calls++
				if userCancel {
					cancel()
				}
				return errors.New("still installing")
			})
			cancel()
			want := context.DeadlineExceeded
			if userCancel {
				want = context.Canceled
			}
			if !errors.Is(err, want) || calls != 1 {
				t.Fatalf("got %v after %d probes, want %v after one probe", err, calls, want)
			}
		}
	})
}
