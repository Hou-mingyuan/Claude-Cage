package mobilelink

import (
	"net/http"
	"net/url"
	"testing"
)

func TestContinueOperationBoundary(t *testing.T) {
	path := "/api/mobile/cli/sessions/73521c54-4c88-4263-89c9-88350fa67968/continue"
	if !allowed(http.MethodPost, &url.URL{Path: path}) ||
		allowed(http.MethodGet, &url.URL{Path: path}) ||
		allowed(http.MethodPost, &url.URL{Path: path, RawQuery: "pid=1"}) {
		t.Fatal("continue must be a paired POST to a specific conversation")
	}
}
