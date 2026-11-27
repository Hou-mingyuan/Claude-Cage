//go:build windows

package workbench

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// currentDesktopSessionID identifies the active interactive Windows session.
// Session 0 is reserved for non-interactive services and is ignored.
func currentDesktopSessionID() string {
	var id uint32
	if err := windows.ProcessIdToSessionId(uint32(os.Getpid()), &id); err != nil || id == 0 {
		return ""
	}
	return fmt.Sprintf("windows-session:%d", id)
}
