package workbench

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func (m *terminalManager) start(cmd *exec.Cmd, workspace, kind string) (*terminalSession, error) {
	return m.startSession(cmd, workspace, kind, claudeSelection{}, nil)
}

// This subprocess uses the same platform PTY implementation as workbench SSH.
func TestTerminalServiceChild(t *testing.T) {
	if os.Getenv("CAGE_TERMINAL_SERVICE_CHILD") != "1" {
		return
	}
	fmt.Println("terminal-ready")
	if os.Getenv("CAGE_TERMINAL_SERVICE_MODE") == "blocked-input" {
		time.Sleep(24 * time.Hour)
		os.Exit(0)
	}
	input := bufio.NewScanner(os.Stdin)
	for input.Scan() {
		line := input.Text()
		fmt.Println("child:" + line)
		if line == "exit" {
			os.Exit(0)
		}
	}
	os.Exit(0)
}

func terminalChildCommand(mode string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestTerminalServiceChild$")
	cmd.Env = append(os.Environ(), "CAGE_TERMINAL_SERVICE_CHILD=1", "CAGE_TERMINAL_SERVICE_MODE="+mode)
	return cmd
}

func terminalInputLine(text string) string {
	if runtime.GOOS == "windows" {
		return text + "\r"
	}
	return text + "\n"
}

func awaitTerminalSignal(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("terminal did not finish")
	}
}

func readTerminalUntil(t *testing.T, sub *terminalSubscription, needle string) string {
	t.Helper()
	text := string(sub.History)
	sub.History = nil
	deadline := time.After(10 * time.Second)
	for !strings.Contains(text, needle) {
		select {
		case chunk, ok := <-sub.Output:
			if !ok {
				t.Fatalf("terminal ended before %q: %q", needle, text)
			}
			text += string(chunk)
		case <-deadline:
			t.Fatalf("terminal did not produce %q: %q", needle, text)
		}
	}
	return text
}

func TestTerminalServiceReconnectKeepsRealProcess(t *testing.T) {
	var manager terminalManager
	t.Cleanup(manager.close)
	session, err := manager.start(terminalChildCommand("echo"), "/project", "shell")
	if err != nil {
		t.Fatal(err)
	}
	first := session.subscribe()
	readTerminalUntil(t, first, "terminal-ready")
	if _, err := io.WriteString(session.file, terminalInputLine("one")); err != nil {
		t.Fatal(err)
	}
	readTerminalUntil(t, first, "child:one")
	first.Close()
	if !manager.hasLive() {
		t.Fatal("detaching viewer ended terminal process")
	}
	if _, err := io.WriteString(session.file, terminalInputLine("two")); err != nil {
		t.Fatal(err)
	}
	second := session.subscribe()
	defer second.Close()
	text := readTerminalUntil(t, second, "child:two")
	if !strings.Contains(text, "child:one") {
		t.Fatalf("reconnection lost history: %q", text)
	}
	if _, err := io.WriteString(session.file, terminalInputLine("exit")); err != nil {
		t.Fatal(err)
	}
	awaitTerminalSignal(t, session.done)
	if manager.hasLive() || !session.summary()["closed"].(bool) {
		t.Fatal("process exit was not published")
	}
	if err := session.file.Wait(); err != nil {
		t.Fatalf("child was killed instead of exiting normally: %v", err)
	}
}

func TestTerminalCloseUnblocksRealPTYInput(t *testing.T) {
	var manager terminalManager
	t.Cleanup(manager.close)
	session, err := manager.start(terminalChildCommand("blocked-input"), "/project", "shell")
	if err != nil {
		t.Fatal(err)
	}
	sub := session.subscribe()
	defer sub.Close()
	readTerminalUntil(t, sub, "terminal-ready")
	if err := session.file.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	writeStarted, writeDone := make(chan struct{}), make(chan struct{})
	go func() {
		close(writeStarted)
		_, _ = io.WriteString(session.file, strings.Repeat("input\n", 1<<20))
		close(writeDone)
	}()
	awaitTerminalSignal(t, writeStarted)
	select {
	case <-writeDone:
		t.Fatal("PTY input unexpectedly drained without a reader")
	case <-time.After(20 * time.Millisecond):
	}
	closed := make(chan struct{})
	go func() { manager.close(); close(closed) }()
	awaitTerminalSignal(t, closed)
	awaitTerminalSignal(t, writeDone)
	awaitTerminalSignal(t, session.done)
	if manager.hasLive() {
		t.Fatal("close returned while terminal was live")
	}
}

func TestTerminalManagerClosesConcurrentCreation(t *testing.T) {
	var manager terminalManager
	t.Cleanup(manager.close)
	begin, created, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var session *terminalSession
	var createErr error
	go func() {
		<-begin
		session, createErr = manager.start(terminalChildCommand("blocked-input"), "/project", "shell")
		close(created)
	}()
	go func() { <-begin; manager.close(); close(closed) }()
	close(begin)
	awaitTerminalSignal(t, created)
	awaitTerminalSignal(t, closed)
	if createErr != nil && !errors.Is(createErr, errTerminalsClosed) {
		t.Fatal(createErr)
	}
	if session != nil {
		awaitTerminalSignal(t, session.done)
	}
	if manager.hasLive() {
		t.Fatal("concurrent create published a live session after close")
	}
	if _, err := manager.start(terminalChildCommand("echo"), "", "shell"); !errors.Is(err, errTerminalsClosed) {
		t.Fatalf("closed manager accepted a process: %v", err)
	}
}

func TestTerminalManagerStopAllowsLaterTerminals(t *testing.T) {
	var manager terminalManager
	t.Cleanup(manager.close)
	first, err := manager.start(terminalChildCommand("blocked-input"), "/first", "shell")
	if err != nil {
		t.Fatal(err)
	}
	manager.closeAll()
	awaitTerminalSignal(t, first.done)
	if manager.get(first.id) != first || manager.hasLive() {
		t.Fatal("stop discarded closed history or kept process alive")
	}
	second, err := manager.start(terminalChildCommand("blocked-input"), "/second", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if second.title != "Claude 2" {
		t.Fatalf("terminal numbering reset: %s", second.title)
	}
	manager.remove(second.id)
	awaitTerminalSignal(t, second.done)
	if manager.get(second.id) != nil {
		t.Fatal("removed terminal remained visible")
	}
}

func TestTerminalManagerEvictsOldestEndedSessions(t *testing.T) {
	for _, liveCount := range []int{0, 3} {
		t.Run(fmt.Sprintf("live-%d", liveCount), func(t *testing.T) {
			var manager terminalManager
			t.Cleanup(manager.close)
			var retained, live []*terminalSession
			for i := 0; i < terminalLimit; i++ {
				session := startControlledTerminal(t, &manager, nil, false, "shell")
				if i < liveCount {
					live = append(live, session)
				} else {
					_ = session.file.Close()
					awaitTerminalSignal(t, session.done)
					retained = append(retained, session)
				}
			}
			for i := 0; i < 3; i++ {
				session := startControlledTerminal(t, &manager, nil, false, "shell")
				live = append(live, session)
				if count := len(manager.snapshot()); count != terminalLimit {
					t.Fatalf("retained %d terminals after publication", count)
				}
				for j, ended := range retained {
					got := manager.get(ended.id)
					if j <= i {
						if got != nil {
							t.Fatalf("oldest ended terminal %d survived eviction %d", j, i)
						}
						ended.mu.Lock()
						dismissed := ended.dismissed
						ended.mu.Unlock()
						if !dismissed {
							t.Fatal("evicted terminal was not dismissed")
						}
					} else if got != ended {
						t.Fatalf("newer ended terminal %d was evicted early", j)
					}
				}
				for _, running := range live {
					if manager.get(running.id) != running || running.ended() {
						t.Fatal("publication evicted or closed a live terminal")
					}
				}
			}
		})
	}
}

func TestTerminalManagerLimitCountsLiveSessions(t *testing.T) {
	var manager terminalManager
	t.Cleanup(manager.close)
	for i := 0; i < terminalLimit; i++ {
		kind := "shell"
		if i == 0 {
			kind = "claude-login"
		}
		startControlledTerminal(t, &manager, nil, i == 0, kind)
	}
	if err := manager.canStart(); !errors.Is(err, errTerminalLimit) || err.Error() != "最多同时运行 12 个终端，请先关闭或归档不用的会话" {
		t.Fatalf("live terminal limit: %v", err)
	}
	started := false
	_, err := manager.startSessionWithPTY(nil, "", "shell", claudeSelection{}, nil, func() (terminalPTY, error) {
		started = true
		return nil, nil
	})
	if !errors.Is(err, errTerminalLimit) || started || len(manager.snapshot()) != terminalLimit {
		t.Fatalf("full manager started a process or changed history: started=%t err=%v", started, err)
	}
}

func TestTerminalManagerCanStartDoesNotEvictHistory(t *testing.T) {
	var manager terminalManager
	t.Cleanup(manager.close)
	var retained []*terminalSession
	for i := 0; i < terminalLimit; i++ {
		session := startControlledTerminal(t, &manager, nil, false, "shell")
		_ = session.file.Close()
		awaitTerminalSignal(t, session.done)
		retained = append(retained, session)
	}
	if err := manager.canStart(); err != nil {
		t.Fatalf("ended sessions blocked admission: %v", err)
	}
	if _, err := manager.startSessionWithPTY(nil, "", "shell", claudeSelection{}, nil, func() (terminalPTY, error) {
		return nil, errors.New("PTY startup failed")
	}); !errors.Is(err, errTerminalStart) {
		t.Fatalf("startup failure: %v", err)
	}
	if len(manager.snapshot()) != terminalLimit || manager.sequence != terminalLimit {
		t.Fatal("admission check or failed startup changed history")
	}
	for _, session := range retained {
		session.mu.Lock()
		dismissed := session.dismissed
		session.mu.Unlock()
		if manager.get(session.id) != session || dismissed {
			t.Fatal("admission check or failed startup evicted a terminal")
		}
	}
}

func TestTerminalManagerReplacementKeepsHistory(t *testing.T) {
	var manager terminalManager
	t.Cleanup(manager.close)
	var retained []*terminalSession
	for i := 0; i < terminalLimit; i++ {
		session := startControlledTerminal(t, &manager, nil, false, "claude-login")
		_ = session.file.Close()
		awaitTerminalSignal(t, session.done)
		retained = append(retained, session)
	}
	previous := retained[len(retained)-1]
	replacement := startControlledTerminal(t, &manager, previous, false, "claude")
	if replacement.id != previous.id || manager.get(previous.id) != replacement || len(manager.snapshot()) != terminalLimit {
		t.Fatal("replacement changed its identity or evicted history")
	}
	for _, session := range retained[:len(retained)-1] {
		if manager.get(session.id) != session {
			t.Fatal("replacement evicted an unrelated ended terminal")
		}
	}
}

func TestTerminalManagerReplacementIgnoresLiveLimit(t *testing.T) {
	var manager terminalManager
	t.Cleanup(manager.close)
	var live []*terminalSession
	for i := 0; i < terminalLimit; i++ {
		live = append(live, startControlledTerminal(t, &manager, nil, false, "shell"))
	}
	previous := newTerminalSession(nil)
	previous.id, previous.kind, previous.closed = "previous-login", "claude-login", true
	close(previous.done)
	manager.mu.Lock()
	manager.sessions[previous.id] = previous
	manager.mu.Unlock()
	replacement := startControlledTerminal(t, &manager, previous, false, "claude")
	if replacement.id != previous.id || manager.get(previous.id) != replacement || len(manager.snapshot()) != terminalLimit+1 {
		t.Fatal("replacement changed its identity or evicted another terminal")
	}
	for _, session := range live {
		if manager.get(session.id) != session || session.ended() {
			t.Fatal("replacement evicted or closed a live terminal")
		}
	}
}

type controlledTerminal struct {
	reader      *io.PipeReader
	writer      *io.PipeWriter
	killed      chan struct{}
	waitStarted chan struct{}
	waitRelease chan struct{}
	killOnce    sync.Once
}

func newControlledTerminal() *controlledTerminal {
	reader, writer := io.Pipe()
	return &controlledTerminal{reader: reader, writer: writer, killed: make(chan struct{}), waitStarted: make(chan struct{}), waitRelease: make(chan struct{})}
}

func newControlledSession() (*terminalSession, *controlledTerminal) {
	file := newControlledTerminal()
	session := newTerminalSession(file)
	go session.run()
	return session, file
}

func startControlledTerminal(t *testing.T, manager *terminalManager, previous *terminalSession, background bool, kind string) *terminalSession {
	t.Helper()
	file := newControlledTerminal()
	close(file.waitRelease)
	session, err := manager.startReplacing(previous, background, nil, "/project", kind, claudeSelection{}, nil, func() (terminalPTY, error) {
		return file, nil
	})
	if err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	return session
}

func (f *controlledTerminal) Read(data []byte) (int, error)  { return f.reader.Read(data) }
func (f *controlledTerminal) Write(data []byte) (int, error) { return len(data), nil }
func (f *controlledTerminal) Resize(uint16, uint16) error    { return nil }
func (f *controlledTerminal) Kill() error                    { f.killOnce.Do(func() { close(f.killed) }); return nil }
func (f *controlledTerminal) Wait() error                    { close(f.waitStarted); <-f.waitRelease; return nil }
func (f *controlledTerminal) Close() error                   { _ = f.writer.Close(); return f.reader.Close() }

func TestTerminalEOFReapsProcessBeforeSessionCompletes(t *testing.T) {
	session, file := newControlledSession()
	manager := terminalManager{sessions: map[string]*terminalSession{"reaping": session}}
	sub := session.subscribe()
	_ = file.writer.Close()
	awaitTerminalSignal(t, file.killed)
	awaitTerminalSignal(t, file.waitStarted)
	if session.ended() || !manager.hasLive() {
		t.Fatal("terminal stopped counting as live before process was reaped")
	}
	select {
	case <-session.done:
		t.Fatal("EOF completed session before process was reaped")
	default:
	}
	closed := make(chan struct{})
	go func() { session.close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("close returned before process Wait")
	default:
	}
	close(file.waitRelease)
	awaitTerminalSignal(t, closed)
	if !session.ended() || manager.hasLive() {
		t.Fatal("reaped terminal remained live")
	}
	if _, ok := <-sub.Output; ok {
		t.Fatal("session completion did not close subscription")
	}
}

func TestTerminalSlowSubscriberDoesNotBlockOutput(t *testing.T) {
	session, file := newControlledSession()
	t.Cleanup(func() { close(file.waitRelease); session.close() })
	slow := session.subscribe()
	fast := session.subscribe()
	defer fast.Close()
	for i := 0; i < 66; i++ {
		chunk := fmt.Sprintf("output-%02d\n", i)
		if _, err := io.WriteString(file.writer, chunk); err != nil {
			t.Fatal(err)
		}
		readTerminalUntil(t, fast, chunk)
	}
	count := 0
	for range slow.Output {
		count++
	}
	if count != 64 {
		t.Fatalf("slow subscriber retained %d chunks", count)
	}
	fast.Close()
	if session.summary()["closed"].(bool) {
		t.Fatal("subscription closure ended session")
	}
	reconnected := session.subscribe()
	defer reconnected.Close()
	if !strings.Contains(string(reconnected.History), "output-65") {
		t.Fatal("slow client prevented recording terminal history")
	}
}

func TestTerminalQueuedInputKeepsAdmissionOrder(t *testing.T) {
	var manager terminalManager
	t.Cleanup(manager.close)
	session, err := manager.start(terminalChildCommand("echo"), "/project", "shell")
	if err != nil {
		t.Fatal(err)
	}
	sub := session.subscribe()
	defer sub.Close()
	readTerminalUntil(t, sub, "terminal-ready")
	for _, line := range []string{"first", "second", "third", "exit"} {
		if err := session.enqueueInput(context.Background(), terminalInputLine(line)); err != nil {
			t.Fatal(err)
		}
	}
	awaitTerminalSignal(t, session.done)
	reconnected := session.subscribe()
	defer reconnected.Close()
	text := string(reconnected.History)
	previous := -1
	for _, line := range []string{"first", "second", "third", "exit"} {
		index := strings.Index(text, "child:"+line)
		if index <= previous {
			t.Fatalf("queued input lost or reordered %s: %q", line, text)
		}
		previous = index
	}
}

func TestTerminalSplitLoginInput(t *testing.T) {
	for _, kind := range []string{"shell", "claude"} {
		session := terminalSession{kind: kind, inputs: make(chan string, 8)}
		for _, input := range []string{"ordinary", "\r", "/lo", "gi", "n"} {
			if err := session.enqueueInput(t.Context(), input); err != nil {
				t.Fatal(err)
			}
			if !session.loginRequested.IsZero() {
				t.Fatalf("%s detected login before it was submitted", kind)
			}
		}
		if err := session.enqueueInput(t.Context(), "\r"); err != nil {
			t.Fatal(err)
		}
		if session.loginRequested.IsZero() != (kind == "shell") {
			t.Fatalf("unexpected login detection for %s", kind)
		}
	}
}

func TestTerminalInputBurstWaitsForWriterInOrder(t *testing.T) {
	session := terminalSession{kind: "shell", inputs: make(chan string, 8)}
	received := make(chan string, 64)
	go func() {
		// A slow PTY writer: the burst below overruns the queue several times.
		for input := range session.inputs {
			time.Sleep(time.Millisecond)
			received <- input
		}
	}()
	var want []string
	for i := range 32 {
		input := fmt.Sprintf("\x1b[?%dc", i)
		want = append(want, input)
		if err := session.enqueueInputWait(t.Context(), input, 5*time.Second); err != nil {
			t.Fatalf("burst input %d: %v", i, err)
		}
	}
	for i, input := range want {
		if got := <-received; got != input {
			t.Fatalf("input %d reordered: %q", i, got)
		}
	}
	close(session.inputs)

	blocked := terminalSession{kind: "shell", inputs: make(chan string, 1)}
	if err := blocked.enqueueInputWait(t.Context(), "a", 0); err != nil {
		t.Fatal(err)
	}
	if err := blocked.enqueueInputWait(t.Context(), "b", 30*time.Millisecond); !errors.Is(err, errTerminalInputFull) {
		t.Fatalf("stalled writer returned %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := blocked.enqueueInputWait(ctx, "b", time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request returned %v", err)
	}
}
