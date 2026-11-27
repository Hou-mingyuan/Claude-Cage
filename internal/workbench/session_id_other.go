//go:build (!darwin || !cgo) && !windows

package workbench

func currentDesktopSessionID() string {
	return ""
}
