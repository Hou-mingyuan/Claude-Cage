package main

// Reserve only Cage's existing public commands and internal launch protocols.
// A new upstream flag or subcommand automatically reaches the guest CLI.
func cageCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "help", "--help", "-h", "version", "--version", "-V", "licenses", "agent", "verify-web-assets", "verify-vm-assets", "guest-os", "interface-language",
		"container-runtime", "container-configure", "container-install-script", "container-network", "container-tap", "server-entry", "diagnostic-event", "diagnostics", "log-relay", "app-update",
		"clipboard-files", "upload-files", "browser-file", "desktop-file", "share-project", "serve-desktop", "serve", "desktop", "prepare-update", "init", "stop", "list", "info",
		"proxy-start", "proxy-stop", "up", "login", "shell", "run", "claude", "doctor", "access", "name", "check", "setup", "update-tools", "hostagent", "start", "restart", "status", "install-cli", "account":
		return true
	case "--debug":
		for _, arg := range args {
			if arg != "--debug" {
				return arg == "hostagent"
			}
		}
	}
	return false
}

func cageArgs(args []string) []string {
	if !cageCommand(args) {
		return append([]string{"claude"}, args...)
	}
	return args
}
