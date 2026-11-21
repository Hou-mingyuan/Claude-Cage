// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package hostagent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// resourceProbeEnd ends each observation on the shared probe connection.
const resourceProbeEnd = "\x1e"

// resourceProbeSession keeps one SSH session open for periodic guest
// observations. A new session per probe costs a connection (or a mux channel),
// the user's shell startup files and several processes every two seconds. The
// guest loop runs the script once per request line; any failure or timeout
// closes the session, and the next request opens a fresh one, so a stalled
// guest is observed exactly as with one session per probe.
type resourceProbeSession struct {
	command func(context.Context, string) *exec.Cmd

	mu      sync.Mutex
	script  string
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	stdin   io.WriteCloser
	replies chan resourceProbeReply
	stderr  *resourceProbeStderr
}

type resourceProbeReply struct {
	data []byte
	err  error
}

func resourceProbeLoop(script string) string {
	inner := "while IFS= read -r _; do\n" + script + "\nprintf '\\036\\n'\ndone"
	return "exec /bin/sh -c '" + strings.ReplaceAll(inner, "'", `'\''`) + "'"
}

func (s *resourceProbeSession) read(ctx context.Context, script string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil || s.script != script {
		s.closeLocked()
		if err := s.startLocked(script); err != nil {
			return nil, err
		}
	}
	if _, err := io.WriteString(s.stdin, "\n"); err != nil {
		stderr := s.closeLocked()
		return nil, fmt.Errorf("resource probe request: %w; stderr=%s", err, stderr)
	}
	select {
	case reply := <-s.replies:
		if reply.err != nil {
			stderr := s.closeLocked()
			if strings.Contains(reply.err.Error(), "exceeded") {
				return nil, reply.err
			}
			return nil, fmt.Errorf("%w; stderr=%s", reply.err, stderr)
		}
		return reply.data, nil
	case <-ctx.Done():
		stderr := s.closeLocked()
		return nil, fmt.Errorf("resource probe read: %w; stderr=%s", ctx.Err(), stderr)
	}
}

func (s *resourceProbeSession) startLocked(script string) error {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := s.command(ctx, resourceProbeLoop(script))
	stderr := &resourceProbeStderr{}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("resource probe stdout: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("resource probe stdin: %w", err)
	}
	if err = cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("resource probe start: %w", err)
	}
	replies := make(chan resourceProbeReply, 1)
	go readResourceProbeReplies(ctx, stdout, replies)
	s.script, s.cmd, s.cancel, s.stdin, s.replies, s.stderr = script, cmd, cancel, stdin, replies, stderr
	return nil
}

// readResourceProbeReplies splits the stream at the end marker. Each
// observation is bounded like the output of a separate probe process.
func readResourceProbeReplies(ctx context.Context, stdout io.Reader, replies chan<- resourceProbeReply) {
	output := bufio.NewReaderSize(stdout, 4096)
	var data []byte
	send := func(reply resourceProbeReply) bool {
		select {
		case replies <- reply:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for {
		line, err := output.ReadSlice('\n')
		if err == nil && string(line) == resourceProbeEnd+"\n" {
			if !send(resourceProbeReply{data: data}) {
				return
			}
			data = nil
			continue
		}
		data = append(data, line...)
		if len(data) > 16384 {
			send(resourceProbeReply{err: errors.New("resource observation exceeded its limit")})
			return
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			send(resourceProbeReply{err: fmt.Errorf("resource probe ended: %w", err)})
			return
		}
	}
}

// closeLocked ends the session and returns its bounded stderr.
func (s *resourceProbeSession) closeLocked() string {
	if s.cmd == nil {
		return ""
	}
	_ = s.stdin.Close()
	s.cancel()
	_ = s.cmd.Wait()
	stderr := string(s.stderr.data)
	s.cmd, s.cancel, s.stdin, s.replies, s.stderr, s.script = nil, nil, nil, nil, nil, ""
	return stderr
}

func (s *resourceProbeSession) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLocked()
}
