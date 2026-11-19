//go:build !windows

package containerenv

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeEngine writes a shell engine that answers ls and inspect for one
// container, logs every call and fails update when updateFails is set.
func fakeEngine(t *testing.T, pidsLimit int64, updateFails bool) (*Runtime, string) {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	config := fixtureConfig()
	config.Engine, config.Socket, config.Executable = "podman", "", filepath.Join(dir, "podman")
	r := newRuntime(t.TempDir(), config)
	inspect := fmt.Sprintf(`[{"Id":"c1","Config":{"Labels":{%q:%q}},"HostConfig":{"PidsLimit":%d,"NanoCpus":%d,"Memory":%d}}]`, profileLabel, r.identity, pidsLimit, int64(config.CPUs)*1e9, config.MemoryBytes)
	update := "exit 0"
	if updateFails {
		update = "echo 'update rejected' >&2; exit 1"
	}
	script := "#!/bin/sh\necho \"$*\" >> '" + calls + "'\ncase \"$1 $2\" in\n" +
		"'container ls') echo c1 ;;\n" +
		"'container inspect') printf '%s' '" + inspect + "' ;;\n" +
		"update*) " + update + " ;;\nesac\n"
	if err := os.WriteFile(config.Executable, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return r, calls
}

func TestFailedResourceUpdateRefusesStart(t *testing.T) {
	r, calls := fakeEngine(t, 1024, true)
	err := r.updateResources(context.Background(), "c1")
	if err == nil || !strings.Contains(err.Error(), "已拒绝启动") || !strings.Contains(err.Error(), "update rejected") {
		t.Fatalf("update failure was not reported as a refused start: %v", err)
	}
	log, _ := os.ReadFile(calls)
	if !strings.Contains(string(log), fmt.Sprintf("update --pids-limit %d --cpus 2 --memory %d --memory-swap %d c1", PIDLimit, 4<<30, 8<<30)) {
		t.Fatalf("engine calls:\n%s", log)
	}
}

func TestCurrentContainerStartsWithoutUpdate(t *testing.T) {
	r, calls := fakeEngine(t, PIDLimit, true)
	if err := r.updateResources(context.Background(), "c1"); err != nil {
		t.Fatal(err)
	}
	if log, _ := os.ReadFile(calls); strings.Contains(string(log), "update") {
		t.Fatalf("updated a current container:\n%s", log)
	}
}

func TestResourceUpdateRejectsAnotherContainer(t *testing.T) {
	r, _ := fakeEngine(t, 1024, false)
	if err := r.updateResources(context.Background(), "c2"); err == nil {
		t.Fatal("updated a container other than the one being started")
	}
}
