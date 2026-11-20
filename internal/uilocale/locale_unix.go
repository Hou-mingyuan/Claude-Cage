//go:build !darwin && !windows

package uilocale

import (
	"os"
	"strings"
)

func hostLanguages() []string {
	var messageLocale string
	for _, name := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if messageLocale = os.Getenv(name); messageLocale != "" {
			break
		}
	}
	if messageLocale == "C" || messageLocale == "POSIX" {
		return nil
	}
	return append(strings.Split(os.Getenv("LANGUAGE"), ":"), messageLocale)
}
