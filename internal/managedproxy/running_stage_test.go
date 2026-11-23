package managedproxy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRunningStageErrorReturnsFixedCodesWithoutSecrets(t *testing.T) {
	secret := "https://user:super-secret@proxy.example.invalid:8443/private"
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"healthy", nil, ""},
		{"configs timeout", &coreError{stage: "readiness.configs", err: fmt.Errorf("GET %s: %w", secret, context.DeadlineExceeded)}, "readiness.configs.timeout"},
		{"group failure", &coreError{stage: "readiness.group", err: errors.New(secret)}, "readiness.group"},
		{"selection mismatch", &coreError{stage: "readiness.selection"}, "readiness.selection"},
		{"local node", fmt.Errorf("bad node %s: %w", secret, ErrLocalProxyNode), "route.local_node"},
		{"generic timeout", fmt.Errorf("dial %s: %w", secret, context.DeadlineExceeded), "timeout"},
		{"generic route error", errors.New(secret), "route"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runningStageError(tc.err)
			if got != tc.want {
				t.Fatalf("runningStageError() = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "secret") || strings.Contains(got, "proxy.example") {
				t.Fatalf("runningStageError leaked sensitive data: %q", got)
			}
		})
	}
}
