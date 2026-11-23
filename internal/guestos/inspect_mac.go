package guestos

import (
	"errors"
	"os"
	"strings"
	"syscall"
)

// macInspection reports the Debian workbench prerequisites of a Mac host.
// Apple Silicon runs Debian through Virtualization.framework; Intel Macs run
// it through the bundled QEMU with Hypervisor.framework. Both need the system
// hypervisor that kern.hv_support reports. It has no build constraint so the
// decision is testable on every platform.
func macInspection(goarch string, hv uint32, probeErr error) Inspection {
	r := Inspection{Guest: Debian13, WorkbenchSupport: "supported"}
	if goarch != "arm64" && goarch != "amd64" {
		r.WorkbenchSupport = "unsupported-host"
		r.Detail = "Mac 发行包面向 Apple Silicon 与 Intel 处理器。"
		return r
	}
	if probeErr != nil {
		target := "macOS 系统虚拟化"
		if goarch == "amd64" {
			target = "Intel Mac 硬件虚拟化"
		}
		reason := strings.TrimSpace(probeErr.Error())
		if errors.Is(probeErr, os.ErrPermission) || errors.Is(probeErr, syscall.EPERM) || errors.Is(probeErr, syscall.EACCES) {
			r.Detail = "当前上下文无权检测 " + target + "能力（读取 kern.hv_support 失败：" + reason + "），这通常表示当前 CLI 运行在受限沙箱中，不能据此判定主机不支持虚拟化或原生桌面后台不可用，也无需重建或重装应用。请在普通终端执行 sysctl kern.hv_support 核对，或通过已运行的原生 Cage 桌面后台检查状态。"
			return r
		}
		r.Detail = "当前上下文无法检测 " + target + "能力（读取 kern.hv_support 失败：" + reason + "），不能据此判定主机不支持虚拟化或原生桌面后台不可用，也无需重建或重装应用。请在普通终端执行 sysctl kern.hv_support 核对，或通过已运行的原生 Cage 桌面后台检查状态。"
		return r
	}
	hypervisor := hv == 1
	r.HostCheckPassed = hypervisor
	switch {
	case hypervisor && goarch == "amd64":
		r.Detail = "已检测到 Debian 工作台所需的 Intel Mac 硬件虚拟化（Hypervisor.framework）。"
	case hypervisor:
		r.Detail = "已检测到 Debian 工作台所需的 macOS 系统虚拟化。"
	case goarch == "amd64":
		r.Detail = "未检测到 Intel Mac 硬件虚拟化（VT-x），无法运行 Debian 工作台。"
	default:
		r.Detail = "未检测到 macOS 系统虚拟化，无法运行 Debian 工作台。"
	}
	return r
}
