package workbench

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type agentNativeItem struct {
	ID         string          `json:"id"`
	Connection string          `json:"connection_id"`
	Request    json.RawMessage `json:"request,omitempty"`
	Status     string          `json:"status"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	created    time.Time
	claimed    time.Time
}

const (
	agentNativeClaimTimeout = 2 * time.Minute
	// agentNativeRunTimeout bounds a claimed request whose result never
	// arrives, e.g. when the window reloaded after receiving it.
	agentNativeRunTimeout = 10 * time.Minute
)

// expireLocked fails a request nobody claimed in time and one claimed but
// never completed, so neither stays outstanding forever. A claimed request
// is never handed out again: its native action may already have run.
func (item *agentNativeItem) expireLocked(now time.Time) {
	switch {
	case item.Status == "pending" && now.Sub(item.created) > agentNativeClaimTimeout:
		item.Status = "failed"
		item.Error = "Desktop did not claim this request in time. No native action was executed."
		item.Request = nil
	case item.Status == "running" && now.Sub(item.claimed) > agentNativeRunTimeout:
		item.Status = "failed"
		item.Error = "Desktop claimed this request but reported no result in time. The native action may or may not have run; inspect state before repeating it."
		item.Request = nil
	}
}

type agentNativeQueue struct {
	mu      sync.Mutex
	items   []*agentNativeItem
	windows map[string]time.Time
	notify  chan struct{}
}

// This queue belongs to the window listener, not the selected workload. It
// invokes the exact installed native bridge, including its platform checks.
func (s *Server) agentNativeRoutes(mux routeRegistrar) {
	mux.HandleFunc("GET /api/agent/native", s.agentNative)
	mux.HandleFunc("POST /api/agent/native", s.agentNative)
	mux.HandleFunc("POST /api/agent/native/window", s.agentNativeWindow)
	mux.HandleFunc("GET /api/agent/native/{id}", s.agentNative)
	mux.HandleFunc("POST /api/agent/native/{id}/complete", s.agentNative)
}

func (s *Server) agentNative(w http.ResponseWriter, r *http.Request) {
	q := &s.agentNativeQueue
	id := r.PathValue("id")
	if r.Method == "POST" && id == "" {
		if !s.hasTrustedView(r) {
			http.Error(w, "Native submission requires the local launcher", 403)
			return
		}
		var in struct {
			Request    json.RawMessage `json:"request"`
			Connection string          `json:"connection_id"`
		}
		if !readJSON(w, r, &in, 12<<20) {
			return
		}
		var request map[string]json.RawMessage
		if json.Unmarshal(in.Request, &request) != nil || request == nil || in.Connection == "" {
			http.Error(w, "Native request and explicit connection_id required", 400)
			return
		}
		var action string
		_ = json.Unmarshal(request["action"], &action)
		if action == "" {
			http.Error(w, "Native action required", 400)
			return
		}
		if s.remote != nil && s.remote.context().ConnectionID != in.Connection {
			http.Error(w, "Connection changed", 409)
			return
		}
		q.mu.Lock()
		defer q.mu.Unlock()
		now := time.Now()
		kept := q.items[:0]
		for _, item := range q.items {
			item.expireLocked(now)
			if now.Sub(item.created) < 30*time.Minute {
				kept = append(kept, item)
			}
		}
		q.items = kept
		if len(q.items) >= 32 {
			for i, item := range q.items {
				if item.Status == "completed" || item.Status == "failed" {
					q.items = append(q.items[:i], q.items[i+1:]...)
					break
				}
			}
		}
		if len(q.items) >= 32 {
			http.Error(w, "Read or finish outstanding native requests before submitting more", 409)
			return
		}
		item := &agentNativeItem{ID: randomToken()[:24], Request: append(json.RawMessage(nil), in.Request...), Connection: in.Connection, Status: "pending", created: now}
		q.items = append(q.items, item)
		if q.notify != nil {
			close(q.notify)
			q.notify = nil
		}
		w.WriteHeader(http.StatusAccepted)
		jsonReply(w, item)
		return
	}
	if r.Method == "GET" && id == "" {
		s.claimAgentNative(w, r)
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if r.Method == "POST" {
		until, ok := q.windows[r.Header.Get("X-Cage-Agent-Window")]
		if !ok || time.Now().After(until) {
			http.Error(w, "Native window proof required", 403)
			return
		}
	}
	for _, item := range q.items {
		if item.ID == id {
			item.expireLocked(time.Now())
			if r.Method == "POST" {
				if item.Status == "completed" || item.Status == "failed" {
					jsonReply(w, map[string]bool{"ok": true})
					return
				}
				if item.Status != "running" {
					http.Error(w, "Native request was not claimed", 409)
					return
				}
				var in struct {
					Result json.RawMessage `json:"result"`
					Error  string          `json:"error"`
				}
				if !readJSON(w, r, &in, 30<<20) {
					return
				}
				item.Result = in.Result
				item.Error = in.Error
				item.Request = nil
				item.Status = "completed"
				if in.Error != "" {
					item.Status = "failed"
				}
				jsonReply(w, map[string]bool{"ok": true})
				return
			}
			if !s.hasTrustedView(r) {
				http.Error(w, "Native results require the local launcher", 403)
				return
			}
			jsonReply(w, item)
			return
		}
	}
	http.Error(w, "Native request unavailable; inspect state before repeating it", 410)
}

func (s *Server) claimAgentNative(w http.ResponseWriter, r *http.Request) {
	q := &s.agentNativeQueue
	proof := r.Header.Get("X-Cage-Agent-Window")
	wait := time.Duration(0)
	if raw := r.URL.Query().Get("wait"); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			if seconds > 25 {
				seconds = 25
			}
			wait = time.Duration(seconds) * time.Second
		}
	}
	var lifeDone <-chan struct{}
	if wait > 0 {
		lifeDone = s.lifetimeContext().Done()
	}
	q.mu.Lock()
	until, ok := q.windows[proof]
	if !ok || time.Now().After(until) {
		q.mu.Unlock()
		http.Error(w, "Native window proof required", 403)
		return
	}
	if r.URL.Query().Get("consumer") != "window" {
		q.mu.Unlock()
		http.Error(w, "Window consumer required", 400)
		return
	}
	conn := requestConnection(r)
	var deadline <-chan time.Time
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		deadline = timer.C
	}
	for {
		// A window that went away while waiting (a reload, a closed window)
		// must not take a request it can no longer receive.
		if r.Context().Err() != nil {
			q.mu.Unlock()
			return
		}
		now := time.Now()
		for _, item := range q.items {
			item.expireLocked(now)
			if item.Status == "pending" && item.Connection == conn {
				item.Status = "running"
				item.claimed = now
				claimed := *item
				q.mu.Unlock()
				jsonReply(w, &claimed)
				return
			}
		}
		if deadline == nil {
			q.mu.Unlock()
			jsonReply(w, map[string]any{})
			return
		}
		if q.notify == nil {
			q.notify = make(chan struct{})
		}
		ch := q.notify
		q.mu.Unlock()
		select {
		case <-ch:
			q.mu.Lock()
			if until, ok := q.windows[proof]; !ok || time.Now().After(until) {
				q.mu.Unlock()
				http.Error(w, "Native window proof required", 403)
				return
			}
		case <-lifeDone:
			jsonReply(w, map[string]any{})
			return
		case <-deadline:
			jsonReply(w, map[string]any{})
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) agentNativeWindow(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Ticket string `json:"ticket"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !s.consumeTicket(in.Ticket) {
		http.Error(w, "A fresh native launch ticket is required", 403)
		return
	}
	q := &s.agentNativeQueue
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.windows == nil {
		q.windows = map[string]time.Time{}
	}
	now := time.Now()
	for proof, until := range q.windows {
		if now.After(until) {
			delete(q.windows, proof)
		}
	}
	if len(q.windows) >= 64 {
		var oldest string
		var earliest time.Time
		for proof, until := range q.windows {
			if oldest == "" || until.Before(earliest) {
				oldest, earliest = proof, until
			}
		}
		delete(q.windows, oldest)
	}
	proof := randomToken()
	q.windows[proof] = now.Add(12 * time.Hour)
	jsonReply(w, map[string]string{"window_proof": proof})
}
