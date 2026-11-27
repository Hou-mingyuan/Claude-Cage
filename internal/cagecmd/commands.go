// Package cagecmd defines the host command names shared by the terminal router
// and the account registry. A saved account name must be usable as a command.
package cagecmd

import (
	"encoding/hex"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// IsCommand reports whether this exact argument is handled by Cage itself.
func IsCommand(word string) bool {
	switch word {
	case "help", "--help", "-h", "version", "--version", "-V", "licenses", "agent", "verify-web-assets", "verify-vm-assets", "guest-os", "interface-language",
		"container-runtime", "container-configure", "container-install-script", "container-network", "container-tap", "server-entry", "diagnostic-event", "diagnostics", "log-relay", "app-update",
		"clipboard-files", "upload-files", "browser-file", "desktop-file", "share-project", "serve-desktop", "serve", "desktop", "prepare-update", "init", "stop", "list", "info",
		"proxy-start", "proxy-stop", "vm-start", "up", "login", "shell", "run", "claude", "doctor", "access", "name", "check", "setup", "update", "update-tools", "hostagent", "start", "restart", "status", "install-cli", "account":
		return true
	}
	return false
}

// AccountNameKey keeps canonically equivalent spellings and case variants
// from selecting different accounts. Display names retain their spelling.
func AccountNameKey(name string) string {
	return norm.NFC.String(cases.Fold().String(norm.NFC.String(name)))
}

func Reserved(name string) bool {
	key := AccountNameKey(name)
	return key == "auto" || IsCommand(key)
}

func ValidAccountName(name string) bool {
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 64 || Reserved(name) {
		return false
	}
	if len(name) == 24 {
		if _, err := hex.DecodeString(name); err == nil {
			return false // reserved for existing immutable Cage IDs
		}
	}
	onlyDigits, mayHaveMark := true, false
	for i, r := range name {
		letter, digit := unicode.IsLetter(r), unicode.IsDigit(r)
		switch {
		case letter || digit:
			mayHaveMark = true
		case i > 0 && strings.ContainsRune("-_.", r):
			mayHaveMark = false
		case mayHaveMark && unicode.IsMark(r):
		default:
			return false
		}
		onlyDigits = onlyDigits && digit
	}
	return !onlyDigits
}
