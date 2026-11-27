package main

import (
	"slices"
	"testing"
)

func TestClaudeArgumentRouting(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		named bool
	}{
		{[]string{"start"}, false},
		{[]string{"login"}, false},
		{[]string{"update", "--check"}, false},
		{[]string{"claude", "update"}, false},
		{[]string{"claude", "--", "login", ""}, false},
		{[]string{"claude", "stop", "abc"}, false},
		{[]string{"--debug", "hostagent", "--pidfile", "x"}, false},
		{[]string{"stop", "--keep-proxy", "base-building"}, false},
		{[]string{"agent", "call", "status"}, false},
		{[]string{"sarah", "-p", "stop", "--output-format", "json"}, true},
		{[]string{"工作", "mcp", "--help"}, true},
		{[]string{"sarah", "--", "-p", ""}, true},
		{[]string{"mcp", "--help"}, true}, // names are resolved by the authenticated controller
	} {
		got, named, err := cageArgs(tc.args)
		if err != nil || named != tc.named || !slices.Equal(got, tc.args) {
			t.Errorf("%q -> %q, named=%v err=%v", tc.args, got, named, err)
		}
	}
}
