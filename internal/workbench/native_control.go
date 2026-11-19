package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

const (
	nativeControlIdle  = 30 * time.Second
	nativeControlRetry = 10 * time.Second
)

// nativeControl carries idempotent polls to a macOS or Windows guest over one
// connection. Opening one per poll costs an SSH key exchange or a relay
// process with its pairing handshake, while file requests, caret status and
// application status arrive every second or two. Requests that change guest
// state keep a connection of their own, so a failed poll can be repeated.
type nativeControl struct {
	slot    chan struct{}
	once    sync.Once
	conn    *nativeWindowConnection
	retryAt time.Time
	idle    *time.Timer
	used    time.Time
}

func (n *nativeControl) acquire(ctx context.Context) bool {
	n.once.Do(func() { n.slot = make(chan struct{}, 1) })
	select {
	case n.slot <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (n *nativeControl) release() { <-n.slot }

func (n *nativeControl) drop() {
	if n.conn != nil {
		n.conn.Close()
		n.conn = nil
	}
}

// exchange sends one request line and returns the reply line. ok is false
// when no reply arrived on the shared connection; the caller then uses a
// connection of its own, which also reports the failure in its usual words.
func (n *nativeControl) exchange(ctx context.Context, open func() (*nativeWindowConnection, error), line []byte) (reply []byte, ok bool) {
	if !n.acquire(ctx) {
		return nil, false
	}
	defer n.release()
	fresh := n.conn == nil
	if fresh {
		if time.Now().Before(n.retryAt) {
			return nil, false
		}
		conn, err := open()
		if err != nil {
			n.retryAt = time.Now().Add(nativeControlRetry)
			return nil, false
		}
		n.conn = conn
	}
	conn := n.conn
	// Closing the connection is the only way to end a blocked read.
	stop := context.AfterFunc(ctx, conn.Close)
	_, err := conn.input.Write(line)
	answered := err == nil && conn.scanner.Scan()
	if answered {
		reply = append([]byte(nil), conn.scanner.Bytes()...)
	}
	if !stop() || !answered {
		// The guest closes a connection it considers idle, and a guest restart
		// ends it as well. The next poll opens another one, unless a new
		// connection already failed: then the guest is not answering yet.
		n.drop()
		if fresh && ctx.Err() == nil {
			n.retryAt = time.Now().Add(nativeControlRetry)
		}
		return nil, false
	}
	n.used = time.Now()
	if n.idle == nil {
		n.idle = time.AfterFunc(nativeControlIdle, n.expire)
	} else {
		n.idle.Reset(nativeControlIdle)
	}
	return reply, true
}

func (n *nativeControl) expire() {
	if !n.acquire(context.Background()) {
		return
	}
	defer n.release()
	if time.Since(n.used) >= nativeControlIdle {
		n.drop()
	}
}

func (n *nativeControl) close() {
	if !n.acquire(context.Background()) {
		return
	}
	defer n.release()
	if n.idle != nil {
		n.idle.Stop()
	}
	n.drop()
	n.retryAt = time.Time{}
}

// windowsDesktopQuick is windowsDesktopRequest for polls that read guest
// state: they share one relay connection instead of starting one each.
func (s *Server) windowsDesktopQuick(ctx context.Context, action string, input map[string]any, output any) error {
	request := map[string]any{}
	for key, value := range input {
		request[key] = value
	}
	request["action"] = action
	line, err := json.Marshal(request)
	if err == nil {
		open := func() (*nativeWindowConnection, error) { return s.windowsWindowConnection(s.lifetimeContext()) }
		if reply, ok := s.nativeControl.exchange(ctx, open, append(line, '\n')); ok {
			return decodeWindowsWindowReply(reply, output)
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return s.windowsDesktopRequest(ctx, action, input, output)
}

func decodeWindowsWindowReply(data []byte, output any) error {
	var status struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &status) != nil {
		return errors.New("客机应用返回了无效响应")
	}
	if status.Error != "" {
		return windowsWindowFailure{code: status.Error}
	}
	if output == nil {
		return nil
	}
	return json.Unmarshal(data, output)
}

// macOSWindowQuick is macOSWindowCall for polls that read guest state: they
// share one SSH channel to the window socket instead of opening one each.
func (s *Server) macOSWindowQuick(parent context.Context, input macOSWindowRequest) ([]byte, error) {
	shared := input
	shared.KeepAlive = true
	line, err := json.Marshal(shared)
	if err == nil && len(line) <= 8000 {
		ctx, cancel := context.WithTimeout(parent, 12*time.Second)
		open := func() (*nativeWindowConnection, error) {
			ctx, cancel := context.WithCancel(s.lifetimeContext())
			return startWindowConnection(s.sshCommand(ctx, "-T", s.sshTarget(), "--", `exec /usr/bin/nc -U "$HOME/Library/Caches/dev.cage.GuestClaudeWindow/window.sock"`), cancel)
		}
		reply, ok := s.nativeControl.exchange(ctx, open, append(line, '\n'))
		cancel()
		if ok && json.Valid(reply) {
			return reply, nil
		}
	}
	if parent.Err() != nil {
		return nil, parent.Err()
	}
	return s.macOSWindowCall(parent, input)
}
