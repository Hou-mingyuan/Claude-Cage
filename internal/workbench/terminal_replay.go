package workbench

import (
	"bytes"
	"context"
	"strings"

	"github.com/klauspost/compress/s2"
)

const (
	terminalHistorySize  = 512 << 10
	terminalReplayBlock  = 32 << 10
	terminalReplayMemory = 1 << 20
	terminalReplayLimit  = 8 << 20
	terminalPausedViews  = 8
)

// The ordinary tail is enough for short pauses. Archive evicted bytes only
// when an authenticated window has explicitly parked a cursor in this PTY.
// Blocks are immutable after publication. Readers can decompress outside mu.
type terminalReplayBlockData struct {
	start uint64
	data  []byte
}

type terminalReplay struct {
	cursors map[string]terminalPausedPosition
	blocks  []terminalReplayBlockData
	bytes   int
	pending []byte
	from    uint64
	stop    func() bool
}

type terminalPausedPosition struct{ offset, stream uint64 }

type terminalViewCursor struct {
	position   uint64
	generation string
	lease      string
}

type terminalViewReplay struct {
	Type       string `json:"type"`
	Start      uint64 `json:"start"`
	End        uint64 `json:"end"`
	Generation string `json:"generation"`
	Lease      string `json:"lease,omitempty"`
	Reset      bool   `json:"reset,omitempty"`
	Gap        bool   `json:"gap,omitempty"`

	stream  uint64
	blocks  []terminalReplayBlockData
	pending []byte
}

func (r terminalViewReplay) writePrefix(write func([]byte) error) error {
	for _, block := range r.blocks {
		data, err := s2.Decode(nil, block.data)
		if err != nil {
			return err
		}
		if err = write(data[max(r.Start, block.start)-block.start:]); err != nil {
			return err
		}
	}
	if len(r.pending) != 0 {
		return write(r.pending)
	}
	return nil
}

func (t *terminalSession) forgetReplayLocked() {
	if t.replay != nil {
		t.replay.stop()
		t.replay = nil
	}
}

func (t *terminalSession) releaseView(epoch context.Context, view terminalViewReplay) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.viewEpoch == epoch && t.replay != nil {
		if parked := t.replay.cursors[view.Lease]; parked.stream > view.stream {
			return
		}
		delete(t.replay.cursors, view.Lease)
		if len(t.replay.cursors) == 0 {
			t.forgetReplayLocked()
		}
	}
}

func (t *terminalSession) parkView(epoch context.Context, view terminalViewReplay, position uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if epoch == nil || epoch.Err() != nil || t.viewEpoch != epoch || view.Lease == "" || position > t.outputEnd || t.kind == "claude-login" || t.dismissed {
		return
	}
	if position < t.outputEnd-uint64(len(t.history)) {
		// A second hide during replay can keep the unconsumed part of its lease.
		if t.replay == nil {
			return
		}
		if old, held := t.replay.cursors[view.Lease]; !held || position < old.offset {
			return
		}
	}
	if t.replay == nil {
		p := &terminalReplay{cursors: make(map[string]terminalPausedPosition)}
		p.stop = context.AfterFunc(epoch, func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			if t.replay == p {
				t.replay = nil
			}
		})
		t.replay = p
	}
	old, held := t.replay.cursors[view.Lease]
	if old.stream > view.stream || !held && len(t.replay.cursors) >= terminalPausedViews {
		return
	}
	t.replay.cursors[view.Lease] = terminalPausedPosition{position, view.stream}
}

func (t *terminalSession) recordOutputLocked(data []byte) {
	t.outputEnd += uint64(len(data))
	t.history = append(t.history, data...)
	excess := len(t.history) - terminalHistorySize
	if excess <= 0 {
		return
	}
	if p := t.replay; p != nil {
		p.save(t.outputEnd-uint64(len(t.history)), t.history[:excess], t.outputEnd)
		if len(p.cursors) == 0 {
			t.forgetReplayLocked()
		}
	}
	t.history = t.history[excess:]
}

func (p *terminalReplay) save(start uint64, evicted []byte, end uint64) {
	oldest := end
	for lease, cursor := range p.cursors {
		if end-cursor.offset > terminalReplayLimit {
			delete(p.cursors, lease)
			continue
		}
		oldest = min(oldest, cursor.offset)
	}
	if oldest >= start+uint64(len(evicted)) {
		return
	}
	if oldest > start {
		evicted = evicted[oldest-start:]
		start = oldest
	}
	if len(p.pending) != 0 && p.from+uint64(len(p.pending)) != start {
		// All readers that needed the earlier interval have returned.
		p.pending = p.pending[:0]
		p.blocks = nil
		p.bytes = 0
	}
	for len(evicted) > 0 {
		if len(p.pending) == 0 {
			p.from = start
		}
		n := min(len(evicted), terminalReplayBlock-len(p.pending))
		p.pending = append(p.pending, evicted[:n]...)
		start += uint64(n)
		evicted = evicted[n:]
		if len(p.pending) == terminalReplayBlock {
			// Encode reserves the worst-case length. Retain only the used bytes.
			block := bytes.Clone(s2.Encode(nil, p.pending))
			p.blocks = append(p.blocks, terminalReplayBlockData{p.from, block})
			p.bytes += len(block)
			p.pending = p.pending[:0]
		}
	}
	for len(p.blocks) > 0 && (p.bytes+len(p.pending) > terminalReplayMemory || p.blocks[0].start+terminalReplayBlock <= oldest) {
		block := p.blocks[0]
		p.bytes -= len(block.data)
		p.blocks[0] = terminalReplayBlockData{}
		p.blocks = p.blocks[1:]
		for lease, cursor := range p.cursors {
			if cursor.offset < block.start+terminalReplayBlock {
				delete(p.cursors, lease)
			}
		}
	}
}

// Subscribe and snapshot under one lock. The cursor counts source bytes, even
// for UTF-8 and terminal escapes split across the hide/show boundary.
func (t *terminalSession) subscribeView(epoch context.Context, cursor *terminalViewCursor) (*terminalSubscription, terminalViewReplay) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.outputEnd = max(t.outputEnd, uint64(len(t.history)))
	start := t.outputEnd - uint64(len(t.history))
	replay := t.viewReplayLocked(epoch, cursor, start)
	return t.subscribeLocked(t.history[max(replay.Start, start)-start:]), replay
}

func (t *terminalSession) viewReplayLocked(epoch context.Context, cursor *terminalViewCursor, start uint64) terminalViewReplay {
	if t.viewEpoch != epoch || t.viewGeneration == "" {
		t.forgetReplayLocked()
		t.viewEpoch = epoch
		// The same terminal ID can be replaced or reused after a restart.
		t.viewGeneration = randomToken()
	}
	t.viewStream++
	result := terminalViewReplay{Type: "replay", Start: start, End: t.outputEnd, Generation: t.viewGeneration, stream: t.viewStream}
	if t.kind == "claude-login" {
		// Only the public notice is replayable from an authorization terminal.
		result.Reset, result.Start, result.End = true, 0, uint64(len(terminalLoginHistory))
		return result
	}
	if cursor == nil || cursor.generation != t.viewGeneration || cursor.position > t.outputEnd || len(cursor.lease) != 64 || strings.Trim(cursor.lease, "0123456789abcdef") != "" {
		result.Lease = randomToken()
		result.Reset = cursor != nil
		return result
	}
	// Keep the viewer's lease stable even when another hide happens before the
	// new handshake arrives. A late close/ack from its older stream cannot
	// release the newer stream's park.
	result.Lease = cursor.lease
	if p := t.replay; p != nil {
		if old, held := p.cursors[cursor.lease]; held && old.offset <= cursor.position {
			p.cursors[result.Lease] = terminalPausedPosition{cursor.position, result.stream}
			if cursor.position < start {
				for _, block := range p.blocks {
					if block.start+terminalReplayBlock > cursor.position {
						result.blocks = append(result.blocks, block)
					}
				}
				if len(p.pending) > 0 {
					result.pending = bytes.Clone(p.pending[max(cursor.position, p.from)-p.from:])
				}
				result.Start = cursor.position
				return result
			}
		}
	}
	if cursor.position < start {
		result.Gap = true
		return result
	}
	result.Start = cursor.position
	return result
}
