package workbench

import (
	"bytes"
	"context"
	"math/rand"
	"testing"
)

func produceTerminalOutput(t *terminalSession, data []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for len(data) > 0 {
		n := min(len(data), 16<<10)
		t.recordOutputLocked(data[:n])
		data = data[n:]
	}
}

func assertTerminalDelta(t *testing.T, session *terminalSession, epoch context.Context, old terminalViewReplay, after uint64, want []byte) terminalViewReplay {
	t.Helper()
	sub, replay := session.subscribeView(epoch, &terminalViewCursor{after, old.Generation, old.Lease})
	defer sub.Close()
	var got bytes.Buffer
	if err := replay.writePrefix(func(block []byte) error { _, err := got.Write(block); return err }); err != nil {
		t.Fatal(err)
	}
	got.Write(sub.History)
	if replay.Reset || replay.Gap || replay.Start != after || !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("delta at %d: got %d bytes, want %d, reset=%t gap=%t", after, got.Len(), len(want), replay.Reset, replay.Gap)
	}
	return replay
}

func TestTerminalParkedViewReplaysEachByteOnce(t *testing.T) {
	epoch, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := newTerminalSession(nil)
	produceTerminalOutput(session, []byte("already rendered"))
	initial, first := session.subscribeView(epoch, nil)
	initial.Close()
	session.parkView(epoch, first, first.End)
	// Include bytes in the middle of UTF-8/control sequences at arbitrary
	// read, compressed-block, and resume boundaries.
	data := bytes.Repeat([]byte("中文\x1b[31m\x1b[0m\r\n"), 90000)
	part := 340177
	produceTerminalOutput(session, data[:part])
	secondSub, second := session.subscribeView(epoch, nil)
	secondSub.Close()
	session.parkView(epoch, second, second.End)
	produceTerminalOutput(session, data[part:])
	if session.replay == nil || len(session.replay.blocks) == 0 {
		t.Fatal("did not exercise archived output")
	}
	active := assertTerminalDelta(t, session, epoch, first, first.End, data)
	// The second viewer can be hidden again before its catch-up finishes.
	active2 := assertTerminalDelta(t, session, epoch, second, second.End, data[part:])
	session.parkView(epoch, active2, second.End+19)
	session.releaseView(epoch, active)
	active2 = assertTerminalDelta(t, session, epoch, active2, second.End+19, data[part+19:])
	session.releaseView(epoch, active2)
	if session.replay != nil {
		t.Fatal("all windows returned but archive survived")
	}
	idle, atTail := session.subscribeView(epoch, &terminalViewCursor{active2.End, active2.Generation, active2.Lease})
	defer idle.Close()
	if atTail.End != active2.End || len(idle.History) != 0 || atTail.Gap {
		t.Fatal("unchanged screen was replayed")
	}
}

func TestTerminalViewFallsBackAfterEitherReplayBudget(t *testing.T) {
	random := make([]byte, 3<<20)
	_, _ = rand.New(rand.NewSource(42)).Read(random)
	for _, data := range [][]byte{random, bytes.Repeat([]byte("x"), terminalReplayLimit+1)} {
		epoch, cancel := context.WithCancel(context.Background())
		session := newTerminalSession(nil)
		sub, old := session.subscribeView(epoch, nil)
		sub.Close()
		session.parkView(epoch, old, 0)
		produceTerminalOutput(session, data)
		if session.replay != nil {
			t.Fatal("archive continued accumulating after exceeding its budget")
		}
		sub, next := session.subscribeView(epoch, &terminalViewCursor{0, old.Generation, old.Lease})
		if !next.Gap || next.Reset || !bytes.Equal(sub.History, data[len(data)-terminalHistorySize:]) {
			t.Fatal("viewer did not receive a marked, bounded history gap")
		}
		sub.Close()
		cancel()
	}
}

func TestLateViewMessagesCannotReleaseANewerPark(t *testing.T) {
	epoch, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := newTerminalSession(nil)
	sub, old := session.subscribeView(epoch, nil)
	sub.Close()
	session.parkView(epoch, old, 0)
	data := bytes.Repeat([]byte("long-output"), 70000)
	produceTerminalOutput(session, data)
	latest := assertTerminalDelta(t, session, epoch, old, 0, data)
	session.parkView(epoch, latest, 32)
	session.releaseView(epoch, old)
	session.parkView(epoch, old, 0)
	again := assertTerminalDelta(t, session, epoch, latest, 32, data[32:])
	session.releaseView(epoch, again)
}

func TestTerminalReplayDoesNotCrossUnlockOrStoreLoginOutput(t *testing.T) {
	epoch, cancel := context.WithCancel(context.Background())
	session := newTerminalSession(nil)
	sub, old := session.subscribeView(epoch, nil)
	sub.Close()
	session.parkView(epoch, old, 0)
	produceTerminalOutput(session, bytes.Repeat([]byte("private"), 200000))
	cancel()
	nextEpoch, closeNext := context.WithCancel(context.Background())
	defer closeNext()
	sub, next := session.subscribeView(nextEpoch, &terminalViewCursor{0, old.Generation, old.Lease})
	sub.Close()
	if session.replay != nil || next.Generation == old.Generation || !next.Reset || len(sub.History) > terminalHistorySize {
		t.Fatal("prior unlock reused a screen or parked lease")
	}
	session.kind = "claude-login"
	sub, login := session.subscribeView(nextEpoch, nil)
	sub.Close()
	session.parkView(nextEpoch, login, login.End)
	if !login.Reset || login.Lease != "" || session.replay != nil || string(sub.History) != terminalLoginHistory {
		t.Fatal("authorization terminal retained replayable output")
	}
}

func TestReplacingPTYResetsAnExistingViewInTheSameUnlock(t *testing.T) {
	epoch, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := newTerminalSession(nil)
	produceTerminalOutput(first, []byte("old"))
	sub, old := first.subscribeView(epoch, nil)
	sub.Close()
	replacement := newTerminalSession(nil)
	produceTerminalOutput(replacement, []byte("new prompt"))
	sub, next := replacement.subscribeView(epoch, &terminalViewCursor{old.End, old.Generation, old.Lease})
	defer sub.Close()
	if !next.Reset || next.Generation == old.Generation || string(sub.History) != "new prompt" {
		t.Fatal("a replacement PTY was appended to the previous session's screen")
	}
}
