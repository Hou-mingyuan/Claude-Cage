//go:build darwin && cgo

package workbench

/*
#include <bsm/audit.h>
*/
import "C"

import "fmt"

// currentDesktopSessionID identifies the active macOS GUI audit session.
// Detached controllers that survive a WindowServer/logout restart keep the
// destroyed Mach bootstrap namespace of the old audit session, breaking XPC
// lookups (scutil, networksetup, opendirectoryd/ssh, launchctl) until restarted
// inside the new GUI session.
func currentDesktopSessionID() string {
	var info C.auditinfo_addr_t
	if C.getaudit_addr(&info, C.int(C.sizeof_auditinfo_addr_t)) != 0 {
		return ""
	}
	if info.ai_asid <= 0 || (info.ai_flags&C.AU_SESSION_FLAG_HAS_GRAPHIC_ACCESS) == 0 {
		return ""
	}
	return fmt.Sprintf("darwin-gui:%d", int64(info.ai_asid))
}
