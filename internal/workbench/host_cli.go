package workbench

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/gorilla/websocket"
)

const hostCLIHeader = "X-Cage-Local-CLI"

func hostCLIProof(token string) string {
	sum := sha256.Sum256([]byte("local terminal\x00" + token))
	return hex.EncodeToString(sum[:])
}

type hostCLIRequest struct {
	Account    string
	Name       string
	Action     string
	Args       []string
	Directory  string
	TTY        bool
	Cols, Rows uint16
}

type hostCLIEvent struct {
	Type       string
	Code       int
	Message    string
	Cols, Rows uint16
}

type hostCLIConn struct {
	*websocket.Conn
	mu sync.Mutex
}

func (c *hostCLIConn) send(kind int, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return c.WriteMessage(kind, data)
}

func (c *hostCLIConn) event(e hostCLIEvent) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return c.send(websocket.TextMessage, b)
}

type hostCLIOutput struct {
	conn    *hostCLIConn
	channel byte
}

// The pipe supervisor acknowledges stdin only after the guest has consumed
// it. Its stdout also carries length-framed native output; stderr stays raw.
type hostCLIPipeOutput struct {
	conn   *hostCLIConn
	buffer bytes.Buffer
}

func (w *hostCLIPipeOutput) Write(data []byte) (int, error) {
	w.buffer.Write(data)
	for w.buffer.Len() > 0 {
		b := w.buffer.Bytes()
		switch b[0] {
		case 'A':
			w.buffer.Next(1)
			if err := w.conn.event(hostCLIEvent{Type: "input-ack"}); err != nil {
				return 0, err
			}
		case 'O':
			if len(b) < 5 {
				return len(data), nil
			}
			n := binary.BigEndian.Uint32(b[1:5])
			if n > 64<<10 {
				return 0, errors.New("无效的客机输出长度")
			}
			if len(b)-5 < int(n) {
				return len(data), nil
			}
			w.buffer.Next(5)
			if _, err := (hostCLIOutput{w.conn, 'o'}).Write(w.buffer.Next(int(n))); err != nil {
				return 0, err
			}
		default:
			return 0, errors.New("无效的客机输出格式")
		}
	}
	return len(data), nil
}

func (w hostCLIOutput) Write(data []byte) (int, error) {
	frame := make([]byte, len(data)+1)
	frame[0] = w.channel
	copy(frame[1:], data)
	if err := w.conn.send(websocket.BinaryMessage, frame); err != nil {
		return 0, err
	}
	return len(data), nil
}

func hostCLIExitCode(err error) int {
	if err == nil {
		return 0
	}
	var status *exec.ExitError
	if errors.As(err, &status) && status.ExitCode() >= 0 {
		return status.ExitCode()
	}
	return 1
}

var errHostCLILogin = errors.New("Claude Code 需要登录 Claude 账号，请运行 cage login")

func (s *Server) hostCLI(w http.ResponseWriter, r *http.Request) {
	ws, err := s.upgrade(w, r)
	if err != nil {
		return
	}
	defer ws.Close()
	ws.SetReadLimit(1 << 20)
	_ = ws.SetReadDeadline(time.Now().Add(15 * time.Second))
	var input hostCLIRequest
	if err := ws.ReadJSON(&input); err != nil {
		return
	}
	_ = ws.SetReadDeadline(time.Time{})
	c := &hostCLIConn{Conn: ws}
	switch input.Action {
	case "run", "run-by-name":
		err = s.hostCLIRun(r.Context(), c, input)
	case "login":
		err = s.hostCLILogin(r.Context(), c)
	case "accounts", "account-status", "use-account", "rename-account", "resolve-name":
		err = s.cliAccountCommand(r.Context(), c, input)
	default:
		err = errors.New("未知的 Cage 终端操作")
	}
	if err != nil {
		kind := "error"
		if errors.Is(err, errHostCLILogin) {
			kind = "login-required"
		}
		_ = c.event(hostCLIEvent{Type: kind, Code: 1, Message: err.Error()})
	}
}

func (s *Server) hostCLIHistoryAccount(ctx context.Context, args []string, project, cwd string) (claudeSelection, error) {
	value, resume := cliOption(args, "--resume", "-r")
	_, cont := cliOption(args, "--continue", "-c")
	if !resume && !cont {
		return s.cliDefaultAccount(project)
	}
	s.claudeAccounts.mu.Lock()
	list, err := s.loadClaudeAccounts()
	s.claudeAccounts.mu.Unlock()
	if err != nil {
		return claudeSelection{}, err
	}
	if len(list.Accounts) == 0 {
		return claudeSelection{}, nil
	}
	accounts := []string{"default"}
	for _, a := range list.Accounts {
		if a.guestID() != "default" {
			accounts = append(accounts, a.guestID())
		}
	}
	roots := map[string]string{cwd: project}
	explicit := resume && value != ""
	if explicit {
		folders, err := s.projectFolders(project)
		if err != nil {
			return claudeSelection{}, err
		}
		for _, folder := range folders {
			roots[guestPath(folder)] = project
		}
	}
	var history []terminalConversation
	if err = s.runCLIChannel(ctx, "discover", map[string]any{"accounts": accounts, "workspaces": roots, "fresh": true}, &history); err != nil {
		return claudeSelection{}, err
	}
	var latest *terminalConversation
	for i := range history {
		h := &history[i]
		if !explicit && h.Workspace != cwd || explicit && h.CLIID != value && h.Title != value {
			continue
		}
		if explicit {
			allowed := false
			for root := range roots {
				allowed = allowed || cliWorkspaceWithin(h.Workspace, root)
			}
			if !allowed {
				continue
			}
		}
		if latest == nil || latest.UpdatedAt < h.UpdatedAt {
			latest = h
		}
	}
	if latest != nil {
		a, _ := matchDiscoveredAccount(list, latest.Config)
		if a.ID != "" {
			return s.selectClaudeAccount(a.ID, false, project)
		}
		if strings.HasSuffix(strings.ReplaceAll(latest.Config, "\\", "/"), "/.claude") {
			return claudeSelection{}, nil
		}
		return claudeSelection{}, errors.New("原会话的 Claude 账号不可用，请在工作台检查")
	}
	if resume && validCLIUUID(value) && len(list.Accounts) > 1 {
		return claudeSelection{}, errors.New("未能确认原会话的账号，请使用 cage account run 指定原账号")
	}
	return s.cliDefaultAccount(project)
}

type hostCLIProcess struct {
	session *terminalSession
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	use     *hostCLIUse
}

func (s *Server) hostCLIStart(ctx context.Context, c *hostCLIConn, in hostCLIRequest) (*hostCLIProcess, error) {
	// A second terminal can wait for the first to finish granting the same
	// project. All following decisions are made against the latest grants.
	if err := s.lockCompletion(ctx); err != nil {
		return nil, err
	}
	defer s.op.Unlock()
	s.mu.Lock()
	ready, update := s.ready && !s.closing, s.guestUpdateRequired
	s.mu.Unlock()
	if !ready || s.busy() {
		return nil, errTerminalNotReady
	}
	var account claudeSelection
	var err error
	explicit := in.Action == "run-by-name" || in.Account != ""
	if explicit {
		list, err := s.cliAccountRoster()
		if err != nil {
			return nil, err
		}
		if in.Action == "run-by-name" {
			account, err = namedCLIAccount(list, in.Name, in.Account)
		} else {
			var id string
			id, err = resolveCLIAccount(list, in.Account)
			if err == nil {
				account, err = list.explicitCodeAccount(id)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if in.TTY {
		if err := s.terminals.canStart(); err != nil {
			return nil, err
		}
	}
	global := cliGlobal(in.Args)
	project, cwd, dir := "", "", ""
	if !global {
		if !filepath.IsAbs(in.Directory) {
			return nil, errors.New("请选择绝对项目路径")
		}
		dir, err = validateWorkspace(in.Directory, s.profile)
		if err != nil {
			return nil, err
		}
		cwd = guestPath(dir)
		groups, err := s.projectGroups()
		if err != nil {
			return nil, err
		}
		_, project = sharedRoot(runProjects{Groups: groups}, dir)
	}
	if !explicit && !cliInfo(in.Args) {
		owner := project
		if owner == "" {
			owner = dir
		}
		account, err = s.hostCLIHistoryAccount(ctx, in.Args, owner, cwd)
		if err != nil {
			if !global && errors.Is(err, errClaudeNeedsLogin) {
				return nil, errHostCLILogin
			}
			return nil, err
		}
	}
	command, verb := cliWords(in.Args)
	changeAuth := command == "auth" && (verb == "login" || verb == "logout")
	use := &hostCLIUse{account: account.ID, changesAuth: changeAuth}
	if !cliInfo(in.Args) {
		use.profile = nativeCLIProfile(account)
		if err := s.checkCLIAccountUse(account, changeAuth); err != nil {
			return nil, err
		}
	}
	if account.ID != "" || changeAuth {
		if other, err := s.terminals.existingProfileLogin(account, changeAuth); err != nil || other != nil {
			if err != nil {
				return nil, err
			}
			return nil, errors.New("该账号正在登录，请等待已有登录完成")
		}
		if changeAuth && s.activeAccountRuns()[account.ID] > 0 {
			return nil, errors.New("该账号仍有运行中的任务，请在任务结束后更改授权")
		}
	}
	if account.ID != "" && update {
		return nil, errGuestUpdateRequired
	}
	if !global && !cliSubcommand(in.Args) && account.ID == "" && account.Subject == "" {
		reading, err := s.probeClaude(ctx, "default")
		if err != nil {
			return nil, err
		}
		if !reading.LoggedIn {
			return nil, errHostCLILogin
		}
	}
	published := false
	defer func() {
		if !published {
			s.releaseCLIUse(use)
		}
	}()
	if !global {
		if project == "" {
			if err = s.shareCLIProject(ctx, dir); err != nil {
				return nil, err
			}
			project = dir
		} else if _, err = s.connectProject(project, false); err != nil {
			return nil, err
		}
		use.workspace = project
		use.folders, err = s.projectFolders(project)
		if err != nil {
			return nil, err
		}
	}
	args := in.Args
	if !global {
		args, err = s.cliArguments(args, dir, use)
		if err != nil {
			return nil, err
		}
	}
	prefix, err := claudeProfileArgs("native", account)
	if err != nil {
		return nil, err
	}
	script := "exec " + shellescape.QuoteCommand(append(prefix, args...))
	if cwd == "" {
		script = "cd \"$HOME\" && " + script
	} else {
		script = "cd -- " + shellescape.Quote(cwd) + " && " + script
	}
	// Only the actual program writes stdout; login-shell startup messages
	// must never corrupt a JSON document or a stream of JSON events.
	script = "exec 3>&1 1>&2; exec /bin/bash -lc " + shellescape.Quote("exec 1>&3 3>&-; "+script)
	if !in.TTY {
		// The SSH connection owns the command, even after Claude has received
		// its stdin EOF. Losing the client or locking Cage ends the guest task.
		script = "exec " + shellescape.QuoteCommand([]string{"/usr/bin/python3", "-I", "-c", mobileSupervisor, "/", "/bin/sh", "-c", script})
	}
	if account.ID != "" {
		script, err = s.routeAccountCommand(ctx, account.ID, script)
		if err != nil {
			return nil, err
		}
	}
	p := &hostCLIProcess{use: use}
	run := s.lifetimeContext()
	flag := "-tt"
	if !in.TTY {
		run, use.cancel = context.WithCancel(ctx)
		flag = "-T"
	}
	s.registerCLIUse(use)
	// Check the account epoch atomically with publication. No server or
	// account-scheduler mutex is acquired while holding the account mutex.
	p.cmd = s.sshCommand(run, flag, s.sshTarget(), "--", script)
	s.account.mu.Lock()
	if !s.mobileAuthorized(ctx) {
		err = errMobileLocked
	} else if in.TTY {
		cols, rows := in.Cols, in.Rows
		if !hostCLISize(cols, rows) {
			cols, rows = 100, 30
		}
		p.session, err = s.terminals.startReplacing(terminalRequest{hostInput: true}, p.cmd, project, "claude", account,
			func() { s.releaseCLIProcess(use) }, func() (terminalPTY, error) { return startTerminal(p.cmd, cols, rows) })
	} else {
		p.cmd.Stdout = &hostCLIPipeOutput{conn: c}
		p.cmd.Stderr = hostCLIOutput{c, 'e'}
		p.stdin, err = p.cmd.StdinPipe()
		if err == nil {
			err = p.cmd.Start()
		}
	}
	s.account.mu.Unlock()
	if err != nil {
		if p.stdin != nil {
			_ = p.stdin.Close()
		}
		if use.cancel != nil {
			use.cancel()
		}
		return nil, err
	}
	published = true
	// A single-use account does not become the project's next automatic pick.
	if !explicit && account.ID != "" {
		s.noteClaudeSelection(account.ID, project)
	}
	return p, nil
}

func hostCLISize(cols, rows uint16) bool { return cols > 1 && cols < 600 && rows > 1 && rows < 400 }

func (s *Server) hostCLIRun(ctx context.Context, c *hostCLIConn, in hostCLIRequest) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p, err := s.hostCLIStart(ctx, c, in)
	if err != nil {
		return err
	}
	if p.session != nil {
		return serveHostCLITerminal(ctx, c, p.session)
	}
	cmd, stdin, use := p.cmd, p.stdin, p.use
	defer s.releaseCLIProcess(use)
	defer use.cancel()
	defer stdin.Close()
	if _, err = io.WriteString(stdin, "{\"binary\":true}\n"); err == nil {
		err = c.event(hostCLIEvent{Type: "ready"})
	}
	if err != nil {
		use.cancel()
		_ = cmd.Wait()
		return err
	}
	// Continue seeing disconnects even when the child stops reading stdin.
	input := make(chan []byte, 1)
	readerDone, writerDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(writerDone)
		for data := range input {
			frame := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
			frame = append(frame, data...)
			if _, err := stdin.Write(frame); err != nil {
				return
			}
			if data == nil {
				return
			}
		}
	}()
	go func() {
		defer close(readerDone)
		defer close(input)
		defer cancel()
		eof := false
		for {
			kind, data, err := c.ReadMessage()
			if err != nil || eof {
				return
			}
			if kind != websocket.BinaryMessage {
				var e hostCLIEvent
				if json.Unmarshal(data, &e) != nil || e.Type != "eof" {
					return
				}
				eof, data = true, nil
			} else if len(data) == 0 || len(data) > 256<<10 {
				return
			}
			select {
			case input <- data:
			default:
				return
			}
		}
	}()
	err = cmd.Wait()
	result := c.event(hostCLIEvent{Type: "exit", Code: hostCLIExitCode(err)})
	_ = c.Close()
	_ = stdin.Close()
	<-readerDone
	<-writerDone
	return result
}

func serveHostCLITerminal(ctx context.Context, c *hostCLIConn, t *terminalSession) error {
	defer func() { t.mu.Lock(); t.hostInput = false; t.mu.Unlock() }()
	sub := t.subscribe()
	defer sub.Close()
	if err := c.event(hostCLIEvent{Type: "terminal"}); err != nil {
		return err
	}
	if len(sub.History) > 0 {
		if _, err := (hostCLIOutput{c, 'h'}).Write(sub.History); err != nil {
			return err
		}
	}
	inputClosed := make(chan struct{})
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		_ = c.Close()
		<-inputClosed
	}()
	go func() {
		defer close(inputClosed)
		for {
			kind, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if kind == websocket.BinaryMessage {
				if err = t.enqueueInputWait(ctx, string(data), terminalInputWait); err != nil {
					return
				}
			} else {
				var e hostCLIEvent
				if json.Unmarshal(data, &e) != nil {
					return
				}
				if e.Type == "resize" && hostCLISize(e.Cols, e.Rows) {
					if t.file.Resize(e.Cols, e.Rows) != nil {
						return
					}
				}
				if e.Type == "eof" {
					return
				}
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-inputClosed:
			return nil
		case data, ok := <-sub.Output:
			if !ok {
				t.mu.Lock()
				closed := t.closed
				t.mu.Unlock()
				if !closed {
					return errors.New("终端输出未能及时接收，可从工作台接回会话")
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-t.done:
				}
				t.mu.Lock()
				code := t.exitCode
				t.mu.Unlock()
				return c.event(hostCLIEvent{Type: "exit", Code: code})
			}
			if _, err := (hostCLIOutput{c, 'o'}).Write(data); err != nil {
				return err
			}
		}
	}
}
