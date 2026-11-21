package guestos

import "testing"

func TestMacInspectionReportsDebianWorkbench(t *testing.T) {
	ready := macInspection("arm64", true)
	if ready.Guest != Debian13 || ready.WorkbenchSupport != "supported" || !ready.HostCheckPassed {
		t.Fatalf("Apple Silicon with hypervisor: %+v", ready)
	}
	missing := macInspection("arm64", false)
	if missing.Guest != Debian13 || missing.WorkbenchSupport != "supported" || missing.HostCheckPassed || missing.Detail == "" {
		t.Fatalf("Apple Silicon without hypervisor: %+v", missing)
	}
	intel := macInspection("amd64", true)
	if intel.Guest != Debian13 || intel.WorkbenchSupport != "supported" || !intel.HostCheckPassed || intel.Detail == "" {
		t.Fatalf("Intel Mac with hypervisor: %+v", intel)
	}
	intelMissing := macInspection("amd64", false)
	if intelMissing.WorkbenchSupport != "supported" || intelMissing.HostCheckPassed || intelMissing.Detail == intel.Detail {
		t.Fatalf("Intel Mac without hypervisor: %+v", intelMissing)
	}
	other := macInspection("386", true)
	if other.WorkbenchSupport != "unsupported-host" || other.HostCheckPassed {
		t.Fatalf("unknown Mac architecture: %+v", other)
	}
}
