package guestos

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
)

func TestMacInspectionReportsDebianWorkbench(t *testing.T) {
	ready := macInspection("arm64", 1, nil)
	if ready.Guest != Debian13 || ready.WorkbenchSupport != "supported" || !ready.HostCheckPassed {
		t.Fatalf("Apple Silicon with hypervisor: %+v", ready)
	}
	missing := macInspection("arm64", 0, nil)
	if missing.Guest != Debian13 || missing.WorkbenchSupport != "supported" || missing.HostCheckPassed || !strings.Contains(missing.Detail, "未检测到 macOS 系统虚拟化") {
		t.Fatalf("Apple Silicon without hypervisor: %+v", missing)
	}
	intel := macInspection("amd64", 1, nil)
	if intel.Guest != Debian13 || intel.WorkbenchSupport != "supported" || !intel.HostCheckPassed || intel.Detail == "" {
		t.Fatalf("Intel Mac with hypervisor: %+v", intel)
	}
	intelMissing := macInspection("amd64", 0, nil)
	if intelMissing.WorkbenchSupport != "supported" || intelMissing.HostCheckPassed || intelMissing.Detail == intel.Detail || !strings.Contains(intelMissing.Detail, "未检测到 Intel Mac 硬件虚拟化") {
		t.Fatalf("Intel Mac without hypervisor: %+v", intelMissing)
	}
	other := macInspection("386", 1, nil)
	if other.WorkbenchSupport != "unsupported-host" || other.HostCheckPassed {
		t.Fatalf("unknown Mac architecture: %+v", other)
	}
}

func TestMacInspectionDistinguishesProbeErrorsFromUnsupportedHost(t *testing.T) {
	for _, goarch := range []string{"arm64", "amd64"} {
		t.Run(goarch, func(t *testing.T) {
			for name, probeErr := range map[string]error{
				"eperm":         syscall.EPERM,
				"wrapped-eperm": fmt.Errorf("sysctl: %w", syscall.EPERM),
				"eacces":        syscall.EACCES,
			} {
				t.Run(name, func(t *testing.T) {
					r := macInspection(goarch, 0, probeErr)
					if r.Guest != Debian13 || r.WorkbenchSupport != "supported" || r.HostCheckPassed {
						t.Fatalf("unexpected inspection for %s/%s: %+v", goarch, name, r)
					}
					if strings.Contains(r.Detail, "未检测到") || strings.Contains(r.Detail, "无法运行 Debian 工作台") {
						t.Fatalf("permission error misreported as missing virtualization: %q", r.Detail)
					}
					for _, want := range []string{"无权检测", "kern.hv_support", probeErr.Error(), "沙箱", "sysctl kern.hv_support", "无需重建或重装"} {
						if !strings.Contains(r.Detail, want) {
							t.Fatalf("detail %q missing %q", r.Detail, want)
						}
					}
				})
			}

			otherErr := errors.New("sysctl unavailable")
			r := macInspection(goarch, 1, otherErr)
			if r.Guest != Debian13 || r.WorkbenchSupport != "supported" || r.HostCheckPassed {
				t.Fatalf("probe error should never report hostCheckPassed=true: %+v", r)
			}
			if strings.Contains(r.Detail, "未检测到") || strings.Contains(r.Detail, "无权检测") {
				t.Fatalf("generic sysctl error misclassified: %q", r.Detail)
			}
			for _, want := range []string{"无法检测", "kern.hv_support", otherErr.Error(), "sysctl kern.hv_support", "无需重建或重装"} {
				if !strings.Contains(r.Detail, want) {
					t.Fatalf("detail %q missing %q", r.Detail, want)
				}
			}
		})
	}
}
