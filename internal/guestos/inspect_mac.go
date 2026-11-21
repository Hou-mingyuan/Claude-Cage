package guestos

// macInspection reports the Debian workbench prerequisites of a Mac host.
// Apple Silicon runs Debian through Virtualization.framework; Intel Macs run
// it through the bundled QEMU with Hypervisor.framework. Both need the system
// hypervisor that kern.hv_support reports. It has no build constraint so the
// decision is testable on every platform.
func macInspection(goarch string, hypervisor bool) Inspection {
	r := Inspection{Guest: Debian13, WorkbenchSupport: "supported"}
	if goarch != "arm64" && goarch != "amd64" {
		r.WorkbenchSupport = "unsupported-host"
		r.Detail = "Mac 发行包面向 Apple Silicon 与 Intel 处理器。"
		return r
	}
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
