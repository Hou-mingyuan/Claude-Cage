package workbench

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNotificationPreferencesStayOnDesktopProfile(t *testing.T) {
	s := testServer(t)
	s.remote = newRemoteGateway(s.profile)
	s.remote.setRemote(true)
	t.Cleanup(s.remote.close)
	desktop := t.TempDir()
	s.appUpdateConfigPath = filepath.Join(desktop, "config.json")
	endpoint := "/api/client/notifications"
	path := filepath.Join(desktop, notificationPreferenceFile)

	if got := request(s, "GET", endpoint, "", "", false).Code; got != 401 {
		t.Fatalf("unauthenticated: %d", got)
	}
	if got := request(s, "POST", endpoint, "https://untrusted.example", `{"turn_completion":"always"}`, true).Code; got != 403 {
		t.Fatalf("foreign origin: %d", got)
	}
	if got := request(s, "POST", endpoint, s.origin, `{"turn_completion":"invalid"}`, true).Code; got != 400 {
		t.Fatalf("invalid turn_completion: %d", got)
	}
	if got := request(s, "POST", endpoint, s.origin, `{"approval":"invalid"}`, true).Code; got != 400 {
		t.Fatalf("invalid approval: %d", got)
	}

	w := request(s, "GET", endpoint, "", "", true)
	var pref NotificationPreferences
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &pref) != nil {
		t.Fatalf("GET defaults: %d %s", w.Code, w.Body.String())
	}
	// A profile that never saved a choice stays silent and never prompts.
	if pref.TurnCompletion != "off" || pref.Approval != "off" {
		t.Fatalf("unexpected defaults: %+v", pref)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reading a fresh profile wrote a preference: %v", err)
	}

	w = request(s, "POST", endpoint, s.origin, `{"turn_completion":"always","approval":"off","sound":false}`, true)
	if w.Code != 200 {
		t.Fatalf("POST notifications: %d %s", w.Code, w.Body.String())
	}
	loaded := loadNotificationPreferences(desktop)
	if loaded.TurnCompletion != "always" || loaded.Approval != "off" || loaded.Sound {
		t.Fatalf("saved preference mismatch: %+v", loaded)
	}
	// An explicit earlier choice is kept as written, not replaced by defaults.
	if err := os.WriteFile(path, []byte(`{"turn_completion":"unfocused","approval":"unfocused","sound":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if loaded = loadNotificationPreferences(desktop); loaded.TurnCompletion != "unfocused" || loaded.Approval != "unfocused" || !loaded.Sound {
		t.Fatalf("saved choice was overwritten: %+v", loaded)
	}
	if _, err := os.Stat(filepath.Join(s.profile, notificationPreferenceFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrote into the workload profile: %v", err)
	}
}

func TestNotificationDispatchDeduplicatesAndStreamsSSE(t *testing.T) {
	s := testServer(t)
	s.terminals.sessions = map[string]*terminalSession{
		"a1b2c3d4e5f60708": {
			id:           "a1b2c3d4e5f60708",
			title:        "Claude 1",
			sharedID:     "shared-1",
			connectionID: "conn-1",
			workspace:    "/tmp/my-project",
			kind:         "claude",
		},
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.notificationEvents(w, r)
	}))
	defer ts.Close()

	// Without the local desktop window's proof, no event stream is opened.
	untrusted, err := ts.Client().Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	untrusted.Body.Close()
	if untrusted.StatusCode != http.StatusForbidden {
		t.Fatalf("untrusted view received the notification stream: %d", untrusted.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: s.cookieName() + "-desktop", Value: trustedViewProof(s.token)})
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != ": connected" {
		t.Fatalf("expected connected comment, got %q err=%v", line, err)
	}
	_, _ = reader.ReadString('\n')

	// Unknown connection or unsupported event kind is ignored.
	if s.dispatchGuestNotification(guestChannelEvent{ID: "x", Event: "turn_completed", Connection: "missing"}) {
		t.Fatal("unknown connection should be ignored")
	}
	if s.dispatchGuestNotification(guestChannelEvent{ID: "x2", Event: "StopFailure", Connection: "conn-1"}) {
		t.Fatal("unsupported event kind should be ignored")
	}

	// First delivery succeeds; duplicate ID is suppressed.
	if !s.dispatchGuestNotification(guestChannelEvent{ID: "conn-1:turn:1", Event: "turn_completed", Connection: "conn-1", CLIID: "cli-1", Seq: 1}) {
		t.Fatal("expected first turn_completed to publish")
	}
	if s.dispatchGuestNotification(guestChannelEvent{ID: "conn-1:turn:1", Event: "turn_completed", Connection: "conn-1", CLIID: "cli-1", Seq: 1}) {
		t.Fatal("duplicate event ID must not publish twice")
	}

	eventLine, err := reader.ReadString('\n')
	if err != nil || strings.TrimSpace(eventLine) != "event: notification" {
		t.Fatalf("expected event header, got %q err=%v", eventLine, err)
	}
	dataLine, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(dataLine, "data: ") {
		t.Fatalf("expected data line, got %q err=%v", dataLine, err)
	}
	var got notificationEvent
	if err := json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(dataLine), "data: ")), &got); err != nil {
		t.Fatalf("decode notification event: %v", err)
	}
	if got.ID != "conn-1:turn:1" || got.Kind != "turn_completed" || got.TerminalID != "a1b2c3d4e5f60708" ||
		got.TabID != "terminal:a1b2c3d4e5f60708" || got.TerminalTitle != "Claude 1" || got.WorkspaceName != "my-project" {
		t.Fatalf("unexpected notification payload: %+v", got)
	}
}

func TestNotificationWatcherSharesChannelStateRoot(t *testing.T) {
	source := cliEventsWatchSource()
	var root string
	for _, line := range strings.Split(cliChannelHelper, "\n") {
		if strings.HasPrefix(line, "ROOT = ") {
			root = line
		}
	}
	if root == "" || !strings.Contains(source, "\n"+root+"\n") {
		t.Fatalf("watcher does not reuse cli_channel.py's ROOT definition")
	}
	if strings.Contains(cliEventsWatchHelper, "cli-channels") {
		t.Fatal("watcher keeps its own copy of the state directory")
	}
}

func TestSameConnectionsIgnoresOrder(t *testing.T) {
	if !sameConnections([]string{"a", "b"}, []string{"b", "a"}) || sameConnections([]string{"a"}, []string{"a", "b"}) || sameConnections([]string{"a", "c"}, []string{"a", "b"}) {
		t.Fatal("connection set comparison is wrong")
	}
}
