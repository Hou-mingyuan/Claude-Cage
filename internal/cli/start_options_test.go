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

func TestStartArgsRejectResourceOptions(t *testing.T) {
	for _, args := range [][]string{{"--cpus", "2"}, {"--memory=4"}, {"--ram", "4"}, {"--reset", "--cpus=1"}} {
		got, err := StartArgs(args)
		if err == nil || got != nil || !strings.Contains(err.Error(), "resource settings in the Cage app") {
			t.Errorf("%q: %q, %v", args, got, err)
		}
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
		{"--disk", "24"}, {"--disk=24"}, {"--ssh-port", "55022"}, {"--ssh-port=0"},
	} {
		if got, err := StartArgs(args); err == nil || got != nil {
			t.Errorf("%q accepted: %q, %v", args, got, err)
		}
	}
}
