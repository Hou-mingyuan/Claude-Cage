package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/vm"
	"github.com/sylvinus/agent-vm/internal/vmname"
)

func TestCageCLIRetainsRunningInstanceUntilTransferMountRestart(t *testing.T) {
	se := newStartEnv(t)
	se.base()
	config := filepath.Join(se.home, "config.json")
	data, err := json.Marshal(cage.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(cage.ConfigEnv, config)
	if code := se.run("run", "true"); code != 0 {
		t.Fatalf("initial environment: %s", se.out())
	}
	instance := se.fake.VMs[vmname.Name(se.proj)]
	old := t.TempDir()
	instance.Mounts[0].Location, instance.Mounts[0].MountPoint = old, old
	se.g.ran = nil
	if code := se.run("run", "true"); code == 0 || !strings.Contains(se.out(), "cage stop") {
		t.Fatalf("old live mount was accepted: %s", se.out())
	}
	if instance.Status != vm.Running || instance.Mounts[0].Location != old || len(se.g.ran) != 0 {
		t.Fatal("restart guard changed the instance or ran a guest command")
	}
	instance.Status = vm.Stopped
	if code := se.run("run", "true"); code != 0 || instance.Mounts[0].Location != se.proj {
		t.Fatalf("stopped instance did not update its transfer mount: %s", se.out())
	}
}
