package workbench

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

//go:embed cli_events_watch.py
var cliEventsWatchHelper string

// The watcher reads the same guest state directory as cli_channel.py. Reuse
// that file's ROOT definition instead of keeping a second copy of the path.
var cliEventsWatchSource = sync.OnceValue(func() string {
	for _, line := range strings.Split(cliChannelHelper, "\n") {
		if strings.HasPrefix(line, "ROOT = ") {
			return "import os\nfrom pathlib import Path\n_controller_context = dict(os.environ)\n" + line + "\n" + cliEventsWatchHelper
		}
	}
	panic("cli_channel.py has no ROOT definition")
})

// Guest events older than this are a backlog from before the stream started
// (app launch, environment reconnect) and are not shown as new banners.
const notificationMaxAge = 60 * time.Second

const notificationPreferenceFile = "notifications.json"

type NotificationPreferences struct {
	TurnCompletion string `json:"turn_completion"`
	Approval       string `json:"approval"`
	Sound          bool   `json:"sound"`
}

// Notifications stay off until the user enables them in Settings, so a fresh
// install never triggers a system permission prompt. A saved file is an
// explicit choice and is always honored as written.
func defaultNotificationPreferences() NotificationPreferences {
	return NotificationPreferences{
		TurnCompletion: "off",
		Approval:       "off",
		Sound:          true,
	}
}

func validNotificationMode(mode string) bool {
	return mode == "unfocused" || mode == "always" || mode == "off"
}

func loadNotificationPreferences(profile string) NotificationPreferences {
	pref := defaultNotificationPreferences()
	data, err := os.ReadFile(filepath.Join(profile, notificationPreferenceFile))
	if err != nil {
		return pref
	}
	if json.Unmarshal(data, &pref) != nil {
		return defaultNotificationPreferences()
	}
	if !validNotificationMode(pref.TurnCompletion) {
		pref.TurnCompletion = "off"
	}
	if !validNotificationMode(pref.Approval) {
		pref.Approval = "off"
	}
	return pref
}

func (s *Server) notificationProfileDir() string {
	if s.appUpdateConfigPath != "" {
		return filepath.Dir(s.appUpdateConfigPath)
	}
	return s.profile
}

func (s *Server) notificationPreferences(w http.ResponseWriter, r *http.Request) {
	profile := s.notificationProfileDir()
	path := filepath.Join(profile, notificationPreferenceFile)
	if r.Method == http.MethodPost {
		pref := loadNotificationPreferences(profile)
		if !readJSON(w, r, &pref, 2048) {
			return
		}
		if !validNotificationMode(pref.TurnCompletion) || !validNotificationMode(pref.Approval) {
			http.Error(w, "请选择有效的通知方式。", http.StatusBadRequest)
			return
		}
		if err := writePrivate(path, pref); err != nil {
			http.Error(w, "无法保存通知设置，请重试。", http.StatusInternalServerError)
			return
		}
		jsonReply(w, pref)
		return
	}
	jsonReply(w, loadNotificationPreferences(profile))
}

type notificationEvent struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	TerminalID    string `json:"terminal_id"`
	TabID         string `json:"tab_id"`
	CLISessionID  string `json:"cli_session_id,omitempty"`
	TerminalTitle string `json:"terminal_title,omitempty"`
	WorkspaceName string `json:"workspace_name,omitempty"`
}

type guestChannelEvent struct {
	ID         string `json:"id"`
	Event      string `json:"event"`
	Connection string `json:"connection"`
	CLIID      string `json:"cli_id"`
	Seq        int    `json:"seq"`
}

type notificationHub struct {
	mu          sync.Mutex
	subscribers map[chan notificationEvent]struct{}
	seen        map[string]struct{}
	seenOrder   []string
	wakeCh      chan struct{}
	watchCancel context.CancelFunc
}

func (h *notificationHub) wake() {
	h.mu.Lock()
	if h.wakeCh == nil {
		h.wakeCh = make(chan struct{}, 1)
	}
	ch := h.wakeCh
	h.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (h *notificationHub) wakeChan() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.wakeCh == nil {
		h.wakeCh = make(chan struct{}, 1)
	}
	return h.wakeCh
}

func (h *notificationHub) setWatchCancel(cancel context.CancelFunc) {
	h.mu.Lock()
	h.watchCancel = cancel
	h.mu.Unlock()
}

func (h *notificationHub) stopWatcher() {
	h.mu.Lock()
	cancel := h.watchCancel
	h.watchCancel = nil
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (h *notificationHub) subscribe() (<-chan notificationEvent, func()) {
	ch := make(chan notificationEvent, 16)
	h.mu.Lock()
	if h.subscribers == nil {
		h.subscribers = map[chan notificationEvent]struct{}{}
	}
	h.subscribers[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subscribers, ch)
			h.mu.Unlock()
		})
	}
}

func (h *notificationHub) publish(ev notificationEvent) bool {
	if ev.ID == "" || ev.Kind == "" || ev.TerminalID == "" {
		return false
	}
	h.mu.Lock()
	if h.seen == nil {
		h.seen = map[string]struct{}{}
	}
	if _, duplicate := h.seen[ev.ID]; duplicate {
		h.mu.Unlock()
		return false
	}
	h.seen[ev.ID] = struct{}{}
	h.seenOrder = append(h.seenOrder, ev.ID)
	if len(h.seenOrder) > 512 {
		delete(h.seen, h.seenOrder[0])
		h.seenOrder = h.seenOrder[1:]
	}
	for ch := range h.subscribers {
		select {
		case ch <- ev:
		default:
		}
	}
	h.mu.Unlock()
	return true
}

func (s *Server) dispatchGuestNotification(raw guestChannelEvent) bool {
	if raw.Event != "turn_completed" && raw.Event != "approval_requested" {
		return false
	}
	if raw.ID == "" || raw.Connection == "" {
		return false
	}
	terminal := s.terminals.forConversation(raw.Connection)
	if terminal == nil {
		return false
	}
	terminal.mu.Lock()
	terminalID := terminal.id
	title := terminal.title
	sharedID := terminal.sharedID
	workspace := terminal.workspace
	closed := terminal.closed
	terminal.mu.Unlock()
	if closed || terminalID == "" {
		return false
	}
	workspaceName := ""
	if workspace != "" {
		base := filepath.Base(strings.TrimRight(workspace, "/\\"))
		if base != "." && base != "/" && base != "\\" {
			workspaceName = base
		}
	}
	return s.notifications.publish(notificationEvent{
		ID:            raw.ID,
		Kind:          raw.Event,
		TerminalID:    terminalID,
		TabID:         "terminal:" + terminalID,
		CLISessionID:  sharedID,
		TerminalTitle: title,
		WorkspaceName: workspaceName,
	})
}

func (s *Server) notificationEvents(w http.ResponseWriter, r *http.Request) {
	// Terminal titles and project names go only to this computer's own desktop
	// window, never to a browser, phone or remote client view.
	if !s.hasTrustedView(r) {
		http.Error(w, "通知仅发送到本机 Cage 窗口", http.StatusForbidden)
		return
	}
	controller := http.NewResponseController(w)
	defer func() {
		if r.Context().Err() == nil {
			_ = controller.SetWriteDeadline(time.Time{})
		}
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	// Subscribe before announcing the stream so an event published right after
	// the window sees ": connected" is not lost.
	events, unsubscribe := s.notifications.subscribe()
	defer unsubscribe()
	_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := w.Write([]byte(": connected\n\n")); err != nil {
		return
	}
	if err := controller.Flush(); err != nil {
		return
	}
	life := s.lifetimeContext()
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-life.Done():
			return
		case <-ticker.C:
			_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := w.Write([]byte(": heartbeat\n\n")); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
		case ev := <-events:
			payload, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := w.Write([]byte("event: notification\ndata: " + string(payload) + "\n\n")); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
		}
	}
}

func (s *Server) monitorTerminalNotifications(ctx context.Context) {
	wake := s.notifications.wakeChan()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		s.mu.Lock()
		ready := s.ready && !s.closing
		s.mu.Unlock()
		epoch, unlocked := s.account.access()
		connections := s.terminals.managedConnections()
		if !ready || !unlocked || len(connections) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-wake:
			case <-ticker.C:
			}
			continue
		}
		watchCtx, cancel := context.WithCancel(ctx)
		s.notifications.setWatchCancel(cancel)
		stopEpoch := context.AfterFunc(epoch, cancel)
		// The guest watcher reads only the connections it was started with.
		// Restart it when terminals open or close; the hub drops repeats.
		var changed atomic.Bool
		go func() {
			for {
				select {
				case <-watchCtx.Done():
					return
				case <-wake:
				case <-ticker.C:
				}
				if !sameConnections(connections, s.terminals.managedConnections()) {
					changed.Store(true)
					cancel()
					return
				}
			}
		}()
		err := s.streamGuestNotifications(watchCtx, connections)
		restarted := changed.Load() && ctx.Err() == nil
		stopEpoch()
		cancel()
		s.notifications.setWatchCancel(nil)
		if err != nil && ctx.Err() == nil && watchCtx.Err() == nil {
			_ = diagnostics.New(s.profile).Record("warn", "cli-notifications", "guest event stream disconnected: "+err.Error())
		}
		if restarted {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-time.After(time.Second):
		}
	}
}

func sameConnections(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, id := range a {
		set[id] = struct{}{}
	}
	for _, id := range b {
		if _, ok := set[id]; !ok {
			return false
		}
	}
	return true
}

func (s *Server) streamGuestNotifications(ctx context.Context, connections []string) error {
	payload, err := json.Marshal(map[string]any{
		"connections": connections,
		"max_age":     notificationMaxAge.Seconds(),
	})
	if err != nil {
		return err
	}
	command := "exec /bin/bash -c " + shellescape.Quote("exec 3>&1 1>&2; exec /bin/bash -lc "+shellescape.Quote("exec 1>&3 3>&-; exec "+shellescape.QuoteCommand([]string{"python3", "-I", "-", string(payload)})))
	cmd := s.sshCommand(ctx, "-T", s.sshTarget(), "--", command)
	cmd.Stdin = strings.NewReader(cliEventsWatchSource())
	cmd.WaitDelay = 2 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 64<<10)
	for scanner.Scan() {
		var raw guestChannelEvent
		if json.Unmarshal(scanner.Bytes(), &raw) != nil {
			continue
		}
		if raw.Event == "ping" {
			continue
		}
		s.dispatchGuestNotification(raw)
	}
	scanErr := scanner.Err()
	waitErr := cmd.Wait()
	if scanErr != nil {
		return scanErr
	}
	return waitErr
}
