package uilocale

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

func hostLanguages() []string {
	// Read the OS display-language order. The web view's default may use
	// the region format or the embedded browser's language instead.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "/usr/bin/defaults", "read", "-g", "AppleLanguages").Output()
	if err != nil {
		return nil
	}
	return strings.FieldsFunc(string(output), func(r rune) bool {
		return strings.ContainsRune("(),\" \t\r\n", r)
	})
}
