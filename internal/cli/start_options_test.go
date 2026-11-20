package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/vm"
	"github.com/sylvinus/agent-vm/internal/vmname"
)

func TestStartArgsResetAndReuse(t *testing.T) {
	se := newStartEnv(t)
	se.base()
	plain, err := StartArgs(nil)
	if err != nil || !slices.Equal(plain, []string{"run", "true"}) {
		t.Fatalf("plain start: %q %v", plain, err)
	}
	if code := se.run(plain...); code != 0 {
		t.Fatalf("initial start: %d %s", code, se.out())
	}
	name := vmname.Name(se.proj)
	original := se.fake.VMs[name]
	if code := se.run(plain...); code != 0 || se.fake.VMs[name] != original || strings.Contains(se.fake.CallLog(), "clone ") || strings.Contains(se.fake.CallLog(), "delete ") {
		t.Fatalf("plain start replaced the VM: %d %s\n%s", code, se.out(), se.fake.CallLog())
	}
	reset, err := StartArgs([]string{"--reset"})
	if err != nil {
		t.Fatal(err)
	}
	if code := se.run(reset...); code != 0 || se.fake.VMs[name] == original || se.fake.VMs[name].Status != vm.Running {
		t.Fatalf("reset did not replace and start the VM: %d %s", code, se.out())
	}
	if !strings.Contains(se.fake.CallLog(), "delete "+name+"\nclone "+vmname.Template+" "+name) {
		t.Fatalf("reset did not delete then clone: %s", se.fake.CallLog())
	}
}

func TestStartArgsApplyResources(t *testing.T) {
	for _, input := range [][]string{
		{"--cpus=1", "--memory", "2", "--disk=24", "--ssh-port", "55022"},
		{"--cpus", "1", "--ram=2", "--disk", "24", "--ssh-port=0"},
	} {
		t.Run(strings.Join(input, " "), func(t *testing.T) {
			se := newStartEnv(t)
			se.base()
			args, err := StartArgs(input)
			if err != nil {
				t.Fatal(err)
			}
			if code := se.run(args...); code != 0 {
				t.Fatalf("start: %d %s", code, se.out())
			}
			inst := se.fake.VMs[vmname.Name(se.proj)]
			port := 55022
			if input[len(input)-1] == "--ssh-port=0" {
				port = 0
			}
			if inst.CPUs != 1 || inst.Memory != 2<<30 || inst.Disk != 24<<30 || inst.SSHPortConfig != port {
				t.Fatalf("resources were not applied: %+v", inst)
			}
		})
	}
}

func TestStartArgsResetPreservesSafety(t *testing.T) {
	se := newStartEnv(t)
	se.base()
	if code := se.run("run", "true"); code != 0 {
		t.Fatal(se.out())
	}
	name := vmname.Name(se.proj)
	original := se.fake.VMs[name]
	args, err := StartArgs([]string{"--reset"})
	if err != nil {
		t.Fatal(err)
	}
	delete(se.fake.VMs, vmname.Template)
	if code := se.run(args...); code != 1 || se.fake.VMs[name] != original || se.fake.CallLog() != "" {
		t.Fatalf("reset without a base changed the VM: %d %s\n%s", code, se.out(), se.fake.CallLog())
	}
	se.base()
	se.fake.Sticky[name] = true
	if code := se.run(args...); code != 1 || se.fake.VMs[name] != original || strings.Contains(se.fake.CallLog(), "clone ") {
		t.Fatalf("reset continued after failed deletion: %d %s\n%s", code, se.out(), se.fake.CallLog())
	}
}

func TestStartArgsRejectUnsupportedOptions(t *testing.T) {
	for _, args := range [][]string{
		{"--unknown"}, {"--reset", "--unknown"}, {"unexpected"}, {"--"}, {"--reset=true"},
		{"--rm"}, {"--scratch"}, {"--readonly"}, {"--tty"},
		{"--unsafe-writable-git=1"}, {"--unsafe-disable-security-prompts"},
		{"--cpus"}, {"--memory=0"}, {"--disk", "10G"}, {"--ssh-port=80"},
	} {
		if got, err := StartArgs(args); err == nil || got != nil {
			t.Errorf("%q accepted: %q, %v", args, got, err)
		}
	}
}
