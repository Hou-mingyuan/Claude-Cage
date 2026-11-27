package workbench

// desktopSessionID returns an identifier for the active interactive desktop
// login session when the host OS ties XPC/system-service reachability to that
// session (macOS GUI audit session, Windows interactive session). Empty means
// either non-interactive execution or a platform without per-session XPC
// namespaces.
var desktopSessionID = currentDesktopSessionID

func sessionChanged(controllerSession string) bool {
	current := desktopSessionID()
	return current != "" && controllerSession != "" && controllerSession != current
}
