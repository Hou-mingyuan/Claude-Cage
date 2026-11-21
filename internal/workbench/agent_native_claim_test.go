package workbench

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func nativeClaimServer(items ...*agentNativeItem) *Server {
	s := &Server{}
	s.agentNativeQueue.windows = map[string]time.Time{"proof": time.Now().Add(time.Hour)}
	s.agentNativeQueue.items = items
	return s
}

// A window that disconnected while its long poll waited must not take the
// request: the reply would be lost and the request stuck in running.
func TestNativeClaimSkipsDisconnectedWindow(t *testing.T) {
	item := &agentNativeItem{ID: "a", Connection: "local", Status: "pending", Request: json.RawMessage(`{"action":"x"}`), created: time.Now()}
	s := nativeClaimServer(item)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, wait := range []string{"5", "0"} {
		r := httptest.NewRequestWithContext(ctx, "GET", "/api/agent/native?consumer=window&connection=local&wait="+wait, nil)
		r.Header.Set("X-Cage-Agent-Window", "proof")
		s.claimAgentNative(httptest.NewRecorder(), r)
		if item.Status != "pending" || item.Request == nil {
			t.Fatalf("wait=%s: disconnected window claimed the request: %+v", wait, item)
		}
	}
	// A live window still receives it.
	r := httptest.NewRequest("GET", "/api/agent/native?consumer=window&connection=local", nil)
	r.Header.Set("X-Cage-Agent-Window", "proof")
	w := httptest.NewRecorder()
	s.claimAgentNative(w, r)
	var got agentNativeItem
	if json.Unmarshal(w.Body.Bytes(), &got) != nil || got.ID != "a" || item.Status != "running" || item.claimed.IsZero() {
		t.Fatalf("live window did not claim: %s %+v", w.Body, item)
	}
}

// A claimed request whose result never arrives is failed after a bound, not
// left running forever, and never handed out a second time.
func TestNativeRunningRequestExpires(t *testing.T) {
	item := &agentNativeItem{ID: "b", Connection: "local", Status: "running", Request: json.RawMessage(`{"action":"x"}`), created: time.Now().Add(-agentNativeRunTimeout - 2*time.Minute), claimed: time.Now().Add(-agentNativeRunTimeout / 2)}
	item.expireLocked(time.Now())
	if item.Status != "running" {
		t.Fatal("a request still within its run bound expired")
	}
	item.claimed = time.Now().Add(-agentNativeRunTimeout - time.Second)
	s := nativeClaimServer(item)
	r := httptest.NewRequest("GET", "/api/agent/native?consumer=window&connection=local", nil)
	r.Header.Set("X-Cage-Agent-Window", "proof")
	w := httptest.NewRecorder()
	s.claimAgentNative(w, r)
	if w.Body.String() != "{}\n" && w.Body.String() != "{}" {
		t.Fatalf("expired running request was handed out again: %s", w.Body)
	}
	if item.Status != "failed" || item.Request != nil || item.Error == "" {
		t.Fatalf("stuck running request not failed: %+v", item)
	}
}
