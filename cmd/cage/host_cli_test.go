package main

import (
	"slices"
	"testing"
)

func TestClaudeArgumentRouting(t *testing.T) {
	for _, tc := range []struct{ in, out []string }{
		{nil, []string{"claude"}},
		{[]string{"start"}, []string{"start"}},
		{[]string{"login"}, []string{"login"}},
		{[]string{"-p", "start"}, []string{"claude", "-p", "start"}},
		{[]string{"mcp", "--help"}, []string{"claude", "mcp", "--help"}},
		{[]string{"-p", "stop", "--output-format", "json"}, []string{"claude", "-p", "stop", "--output-format", "json"}},
		{[]string{"--", "login", ""}, []string{"claude", "--", "login", ""}},
		{[]string{"claude", "stop", "abc"}, []string{"claude", "stop", "abc"}},
		{[]string{"--debug", "hostagent", "--pidfile", "x"}, []string{"--debug", "hostagent", "--pidfile", "x"}},
		{[]string{"stop", "--keep-proxy", "base-building"}, []string{"stop", "--keep-proxy", "base-building"}},
		{[]string{"agent", "call", "status"}, []string{"agent", "call", "status"}},
	} {
		got := cageArgs(tc.in)
		if !slices.Equal(got, tc.out) {
			t.Errorf("%q -> %q, want %q", tc.in, got, tc.out)
		}
	}
}
