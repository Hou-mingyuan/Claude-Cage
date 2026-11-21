package workbench

import (
	"bufio"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"al.essio.dev/pkg/shellescape"
)

//go:embed guest-session.py
var guestSessionServer string

const (
	guestSessionReady   = "CAGE-GUEST-SESSION 1"
	guestSessionIdle    = 30 * time.Second
	guestSessionRetry   = 30 * time.Second
	guestSessionStartup = 5 * time.Second
	// guestSessionTimeouts consecutive timed-out requests, with no reply in
	// between, mark the transport as stalled: the connection is closed and
	// callers use connections of their own until guestSessionRetry passes.
	guestSessionTimeouts = 2
)

// guestSessionOutputMaximum matches OUTPUT_MAXIMUM in guest-session.py.
const guestSessionOutputMaximum = (4 << 20) + 256

var errGuestSessionEnded = errors.New("guest control session ended")

type guestSessionReply struct {
	code           int
	stdout, stderr []byte
}

// guestSession carries control scripts to a Linux guest over one SSH
// connection. Opening a connection per script costs a key exchange, a login
// shell and several processes on both sides, while status polls and display
// changes arrive every second or two. It closes after a short idle period.
type guestSession struct {
	mu      sync.Mutex
	conn    *guestSessionConn
	retryAt time.Time
	idle    *time.Timer
}

type guestSessionConn struct {
	cancel  context.CancelFunc
	stdin   io.WriteCloser
	writing sync.Mutex
	mu      sync.Mutex
	waiting map[uint64]chan guestSessionReply
	next    uint64
	ended   chan struct{}
	// timeouts counts requests that timed out since the last reply.
	timeouts atomic.Int32
}

func (c *guestSessionConn) close() { c.cancel(); _ = c.stdin.Close() }

func (c *guestSessionConn) alive() bool {
	select {
	case <-c.ended:
		return false
	default:
		return true
	}
}

func (c *guestSessionConn) receive(output *bufio.Reader, done func()) {
	defer done()
	defer close(c.ended)
	defer c.close()
	for {
		line, err := output.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		if len(fields) != 5 || fields[0] != "D" {
			return
		}
		id, idErr := strconv.ParseUint(fields[1], 10, 64)
		code, codeErr := strconv.Atoi(fields[2])
		outSize, outErr := strconv.Atoi(fields[3])
		errSize, errErr := strconv.Atoi(fields[4])
		if idErr != nil || codeErr != nil || outErr != nil || errErr != nil || outSize < 0 || outSize > guestSessionOutputMaximum || errSize < 0 || errSize > 1<<16 {
			return
		}
		data := make([]byte, outSize+errSize)
		if _, err = io.ReadFull(output, data); err != nil {
			return
		}
		c.timeouts.Store(0)
		c.mu.Lock()
		waiter := c.waiting[id]
		delete(c.waiting, id)
		c.mu.Unlock()
		if waiter != nil {
			waiter <- guestSessionReply{code: code, stdout: data[:outSize], stderr: data[outSize:]}
		}
	}
}

// openGuestSession starts the server on the guest. The command's stdin and
// stdout belong to the returned connection.
func openGuestSession(parent context.Context, command func(context.Context) *exec.Cmd) (*guestSessionConn, error) {
	ctx, cancel := context.WithCancel(parent)
	cmd := command(ctx)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	conn := &guestSessionConn{cancel: cancel, stdin: stdin, waiting: map[uint64]chan guestSessionReply{}, ended: make(chan struct{})}
	output := bufio.NewReaderSize(stdout, 64<<10)
	ready := make(chan error, 1)
	go func() {
		// A login shell may print before the server starts.
		for skipped := 0; skipped < 64; skipped++ {
			line, err := output.ReadString('\n')
			if err != nil {
				ready <- err
				return
			}
			if strings.TrimSpace(line) == guestSessionReady {
				ready <- nil
				return
			}
		}
		ready <- errors.New("guest control session did not start")
	}()
	select {
	case err = <-ready:
	case <-time.After(guestSessionStartup):
		err = errors.New("guest control session start timed out")
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		conn.close()
		_ = cmd.Wait()
		return nil, err
	}
	go conn.receive(output, func() { _ = cmd.Wait() })
	return conn, nil
}

// run sends one script. sent reports whether the guest may have started it:
// when false, the caller can safely use a connection of its own instead.
func (g *guestSession) run(ctx context.Context, lifetime context.Context, command func(context.Context) *exec.Cmd, script string) (reply guestSessionReply, sent bool, err error) {
	return g.runInput(ctx, lifetime, command, script, nil, 0)
}

// runInput also passes standard input; a positive limit replaces the
// default 64 KiB output bound (the guest stops a script that exceeds it).
func (g *guestSession) runInput(ctx context.Context, lifetime context.Context, command func(context.Context) *exec.Cmd, script string, input []byte, limit int) (reply guestSessionReply, sent bool, err error) {
	header := ""
	if input != nil || limit > 0 {
		if limit <= 0 {
			limit = (64 << 10) + 256
		}
		if limit > guestSessionOutputMaximum || len(input) > 1<<20 {
			return reply, false, errors.New("guest control request exceeded its limit")
		}
		header = fmt.Sprintf(" %d %d", len(input), limit)
	}
	g.mu.Lock()
	conn := g.conn
	if conn == nil || !conn.alive() {
		g.conn = nil
		if time.Now().Before(g.retryAt) {
			g.mu.Unlock()
			return reply, false, errGuestSessionEnded
		}
		if conn, err = openGuestSession(lifetime, command); err != nil {
			g.retryAt = time.Now().Add(guestSessionRetry)
			g.mu.Unlock()
			return reply, false, err
		}
		g.conn = conn
	}
	if g.idle != nil {
		g.idle.Stop()
	}
	conn.mu.Lock()
	conn.next++
	id, waiter := conn.next, make(chan guestSessionReply, 1)
	conn.waiting[id] = waiter
	conn.mu.Unlock()
	g.mu.Unlock()
	defer g.release(conn)
	forget := func() { conn.mu.Lock(); delete(conn.waiting, id); conn.mu.Unlock() }
	conn.writing.Lock()
	_, err = io.WriteString(conn.stdin, fmt.Sprintf("R %d %d%s\n%s%s", id, len(script), header, script, input))
	conn.writing.Unlock()
	if err != nil {
		// A partial request never runs: the server needs its full length.
		forget()
		conn.close()
		return reply, false, err
	}
	select {
	case reply = <-waiter:
		return reply, true, nil
	case <-ctx.Done():
		forget()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && conn.timeouts.Add(1) >= guestSessionTimeouts {
			// A half-dead transport would make every later request wait for
			// its full timeout. Drop it; callers fall back to SSH of their own.
			g.abandon(conn)
			return reply, true, ctx.Err()
		}
		// The cancel write must not hold up the caller when the transport
		// stops reading; closing the connection ends a blocked write.
		go func() {
			conn.writing.Lock()
			defer conn.writing.Unlock()
			_, _ = io.WriteString(conn.stdin, fmt.Sprintf("C %d\n", id))
		}()
		return reply, true, ctx.Err()
	case <-conn.ended:
		forget()
		return reply, true, errGuestSessionEnded
	}
}

// abandon closes a stalled connection and holds off reopening it, so the
// callers use connections of their own meanwhile.
func (g *guestSession) abandon(conn *guestSessionConn) {
	g.mu.Lock()
	if g.conn == conn {
		g.conn = nil
		g.retryAt = time.Now().Add(guestSessionRetry)
	}
	g.mu.Unlock()
	conn.close()
}

func (g *guestSession) release(conn *guestSessionConn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	conn.mu.Lock()
	busy := len(conn.waiting) > 0
	conn.mu.Unlock()
	if busy || g.conn != conn {
		return
	}
	if g.idle != nil {
		g.idle.Stop()
	}
	g.idle = time.AfterFunc(guestSessionIdle, func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		conn.mu.Lock()
		busy := len(conn.waiting) > 0
		conn.mu.Unlock()
		if g.conn == conn && !busy {
			g.conn = nil
			conn.close()
		}
	})
}

// close ends the connection; a script still running on it reports an error.
func (g *guestSession) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.idle != nil {
		g.idle.Stop()
	}
	if g.conn != nil {
		g.conn.close()
		g.conn = nil
	}
	g.retryAt = time.Time{}
}

func (s *Server) guestSessionCommand(ctx context.Context) *exec.Cmd {
	return s.sshCommand(ctx, "-T", s.sshTarget(), "--", "exec /usr/bin/python3 -I -u -c "+shellescape.Quote(guestSessionServer))
}

// guestShared runs a script on the shared connection. ok is false when the
// script was not sent and the caller should open a connection of its own.
func (s *Server) guestShared(ctx context.Context, script string) (reply guestSessionReply, ok bool, err error) {
	reply, sent, err := s.controlSession.run(ctx, s.lifetimeContext(), s.guestSessionCommand, script)
	return reply, sent, err
}

// guestQuick is guest for idempotent scripts that run often or while the
// user waits: status polls and display changes share one SSH connection.
// The scripts must not depend on environment changed after it was opened.
func (s *Server) guestQuick(ctx context.Context, script string) ([]byte, error) {
	reply, ok, err := s.guestShared(ctx, script)
	if !ok {
		return s.guest(ctx, script)
	}
	if err != nil {
		return nil, err
	}
	if len(reply.stdout) > 64<<10 {
		return nil, errors.New("guest control response exceeded its limit")
	}
	if reply.code != 0 {
		return nil, fmt.Errorf("guest control script: exit status %d", reply.code)
	}
	return reply.stdout, nil
}
