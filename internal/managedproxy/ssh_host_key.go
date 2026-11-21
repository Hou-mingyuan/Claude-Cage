package managedproxy

import "strings"

// sshHostKeyAlgorithms are the host key algorithms that can present a pinned
// public key of each type. An RSA key signs with SHA-2 or, on old servers,
// SHA-1, so all three names verify the same key.
var sshHostKeyAlgorithms = map[string][]string{
	"ssh-ed25519":                        {"ssh-ed25519"},
	"ecdsa-sha2-nistp256":                {"ecdsa-sha2-nistp256"},
	"ecdsa-sha2-nistp384":                {"ecdsa-sha2-nistp384"},
	"ecdsa-sha2-nistp521":                {"ecdsa-sha2-nistp521"},
	"sk-ssh-ed25519@openssh.com":         {"sk-ssh-ed25519@openssh.com"},
	"sk-ecdsa-sha2-nistp256@openssh.com": {"sk-ecdsa-sha2-nistp256@openssh.com"},
	"ssh-rsa":                            {"rsa-sha2-512", "rsa-sha2-256", "ssh-rsa"},
}

// pinSSHHostKeyAlgorithms limits an SSH node with pinned host keys to the
// algorithms of those keys. Otherwise the server may present another of its
// host keys, for example ECDSA when only Ed25519 is pinned, and the pinned
// check rejects the connection although the server is the expected one.
// An explicit host-key-algorithms list, and keys of an unknown type, are left
// unchanged; the pinned check itself is never relaxed.
func pinSSHHostKeyAlgorithms(node map[string]any) {
	if node["type"] != "ssh" {
		return
	}
	if _, set := node["host-key-algorithms"]; set {
		return
	}
	var keys []string
	switch value := node["host-key"].(type) {
	case string:
		keys = []string{value}
	case []any:
		for _, item := range value {
			key, ok := item.(string)
			if !ok {
				return
			}
			keys = append(keys, key)
		}
	default:
		return
	}
	var algorithms []any
	seen := map[string]bool{}
	for _, key := range keys {
		fields := strings.Fields(key)
		if len(fields) == 0 {
			continue
		}
		names, known := sshHostKeyAlgorithms[fields[0]]
		if !known {
			return
		}
		for _, name := range names {
			if !seen[name] {
				seen[name] = true
				algorithms = append(algorithms, name)
			}
		}
	}
	if len(algorithms) > 0 {
		node["host-key-algorithms"] = algorithms
	}
}
