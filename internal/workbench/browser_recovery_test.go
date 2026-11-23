package workbench

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDownloadRetryUsesAuthenticatedFixedGuestAction(t *testing.T) {
	s := testServer(t)
	s.ready = true
	s.credentials.Token = "guest-test-token"
	called := 0
	lastURL := ""
	s.browserControl = browserClientForTest(t, s, browserTransport(func(r *http.Request) (*http.Response, error) {
		called++
		lastURL = r.URL.String()
		if (lastURL != "http://127.0.0.1:18500/download-retry" && lastURL != "http://127.0.0.1:18500/download-discard") || r.Header.Get("Authorization") != "Bearer guest-test-token" {
			t.Fatal("download action did not use the fixed authenticated guest endpoint", lastURL)
		}
		var input struct{ ID string }
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.ID != "download-id" {
			t.Fatal("download action lost the selected download", err)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
	}))
	for _, action := range []string{"download-retry", "download-discard", "editor-restart"} {
		if w := request(s, "POST", "/api/browser/"+action, s.origin, `{}`, false); w.Code != 401 {
			t.Fatal("recovery accepted without session", action, w.Code)
		}
		if w := request(s, "POST", "/api/browser/"+action, "https://example.com", `{}`, true); w.Code != 403 {
			t.Fatal("cross-origin recovery accepted", action, w.Code)
		}
		if w := request(s, "POST", "/api/browser/"+action, s.origin, `{"unit":"ssh.service"}`, true); w.Code != 400 {
			t.Fatal("arbitrary service parameters accepted", action, w.Code)
		}
	}
	if called != 0 {
		t.Fatal("rejected request reached the guest")
	}
	if w := request(s, "POST", "/api/browser/download-retry", s.origin, `{"id":"download-id"}`, true); w.Code != 200 || called != 1 || lastURL != "http://127.0.0.1:18500/download-retry" {
		t.Fatal("authorized retry failed", w.Code, w.Body.String())
	}
	if w := request(s, "POST", "/api/browser/download-discard", s.origin, `{"id":"download-id"}`, true); w.Code != 200 || called != 2 || lastURL != "http://127.0.0.1:18500/download-discard" {
		t.Fatal("authorized discard failed", w.Code, w.Body.String())
	}
}
