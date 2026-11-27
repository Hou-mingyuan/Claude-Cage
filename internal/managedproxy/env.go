package managedproxy

import "strings"

// The core dials nodes and downloads providers itself; a proxy configured for
// the user's session would send that traffic through another proxy first.
var hostProxyEnvNames = []string{"http_proxy", "https_proxy", "all_proxy", "ftp_proxy", "no_proxy"}

// WithoutHostProxyEnv returns env without the host's proxy variables, matched
// in any letter case.
func WithoutHostProxyEnv(env []string) []string {
	kept := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if !hostProxyEnvName(key) {
			kept = append(kept, entry)
		}
	}
	return kept
}

func hostProxyEnvName(key string) bool {
	for _, name := range hostProxyEnvNames {
		if strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}
