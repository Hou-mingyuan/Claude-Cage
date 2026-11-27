//go:build darwin || windows

package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeUpdateRequiresApprovalAndHandoff(t *testing.T) {
	for _, tc := range []struct {
		scenario, version, problem string
		yes                        bool
	}{
		{"success", "1.1.0", "", true},
		{"success", "1.1.0", "非交互安装需要 --yes", false},
		{"early-handoff", "1.1.0", "未收到本次终端确认", true},
		{"success", "1.2.0", "待安装的版本与此次检查不一致", true},
		{"preparing", "1.1.0", "尚未收到安装程序接手", true},
		{"busy", "1.1.0", "请先执行 cage stop", true},
	} {
		t.Run(fmt.Sprintf("%s/%s/yes=%t", tc.scenario, tc.version, tc.yes), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			in, err := os.Open(os.DevNull)
			if err != nil {
				t.Fatal(err)
			}
			defer in.Close()
			cmd := exec.Command(os.Args[0], "-test.run=^TestNativeUpdateProcess$")
			cmd.Env = append(os.Environ(), "CAGE_TEST_UPDATE_SCENARIO="+tc.scenario)
			var out, notices bytes.Buffer
			err = runNativeUpdate(ctx, cmd, tc.version, in, &out, &notices, tc.yes)
			if tc.problem == "" {
				if err != nil || !strings.Contains(out.String(), "已交给安装程序") {
					t.Fatalf("stdout %q, error %v", out.String(), err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.problem) || out.Len() != 0 {
				t.Fatalf("stdout %q, error %v, want %s", out.String(), err, tc.problem)
			}
		})
	}
}

func TestNativeUpdateProcess(t *testing.T) {
	scenario := os.Getenv("CAGE_TEST_UPDATE_SCENARIO")
	if scenario == "" {
		t.Skip("update helper subprocess")
	}
	if scenario != "early-handoff" {
		fmt.Println(`{"phase":"ready","latestVersion":"1.1.0"}`)
		answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil || answer != "INSTALL\n" {
			os.Exit(1)
		}
	}
	switch scenario {
	case "preparing":
		fmt.Println(`{"phase":"installing"}`)
	case "busy":
		fmt.Println(`{"phase":"waiting","message":"请先执行 cage stop"}`)
	default:
		fmt.Println(`{"phase":"handed_off"}`)
	}
	os.Exit(0)
}

func TestNativeUpdateTargetsSameInstallation(t *testing.T) {
	for _, tc := range []struct{ platform, cli, launcher string }{
		{"darwin", "/test/Cage A.app/Contents/Resources/cage", "/test/Cage A.app/Contents/MacOS/Cage"},
		{"windows", "/test/Cage A/Resources/Cage.exe", "/test/Cage A/Cage.exe"},
		{"darwin", "/test/bin/cage", ""},
		{"windows", "/test/bin/cage.exe", ""},
	} {
		got, err := nativeUpdateLauncher(filepath.FromSlash(tc.cli), tc.platform)
		if got != filepath.FromSlash(tc.launcher) || (err == nil) != (tc.launcher != "") {
			t.Errorf("launcher for %s: %s, %v", tc.cli, got, err)
		}
	}
}
