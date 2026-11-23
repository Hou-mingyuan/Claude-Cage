package workbench

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/sylvinus/agent-vm/internal/desktop"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/remotehost"
	"golang.org/x/net/http/httpguts"
)

const (
	remoteApprovalProtocol   = "cage-approval-v1"
	remoteApprovalFrameLimit = 64 << 10
	remoteApprovalPingEvery  = 20 * time.Second
)

// remoteApprovalPrompt carries structured call facts from the remote server to
// the connected desktop client. The client validates all strings locally and
// formats its own dialog wording with its locally stored host name.
type remoteApprovalPrompt struct {
	Kind      string `json:"kind"` // "command" or "tool"
	Plugin    string `json:"plugin,omitempty"`
	Tool      string `json:"tool,omitempty"`
	Project   string `json:"project,omitempty"`
	Dir       string `json:"dir,omitempty"`
	Command   string `json:"command,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	FromView  bool   `json:"from_view,omitempty"`
}

type approvalFrame struct {
	Type       string                `json:"type"`
	ID         string                `json:"id,omitempty"`
	Prompt     *remoteApprovalPrompt `json:"prompt,omitempty"`
	TimeoutSec int                   `json:"timeout_sec,omitempty"`
	Scope      string                `json:"scope,omitempty"`
	Answered   bool                  `json:"answered,omitempty"`
}

type approvalReply struct {
	scope    desktop.Scope
	answered bool
}

type remoteApprovalConn struct {
	stream  io.ReadWriteCloser
	writeMu sync.Mutex
	closed  chan struct{}
	once    sync.Once
	mu      sync.Mutex
	pending map[string]chan approvalReply
}

func newRemoteApprovalConn(stream io.ReadWriteCloser) *remoteApprovalConn {
	return &remoteApprovalConn{
		stream:  stream,
		closed:  make(chan struct{}),
		pending: map[string]chan approvalReply{},
	}
}

func (c *remoteApprovalConn) close() {
	c.once.Do(func() {
		close(c.closed)
		_ = c.stream.Close()
		c.mu.Lock()
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.mu.Unlock()
	})
}

func (c *remoteApprovalConn) send(frame approvalFrame) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if len(data) > remoteApprovalFrameLimit {
		return errors.New("approval frame too large")
	}
	data = append(data, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	select {
	case <-c.closed:
		return io.ErrClosedPipe
	default:
	}
	_, err = c.stream.Write(data)
	return err
}

func (c *remoteApprovalConn) ask(ctx context.Context, prompt remoteApprovalPrompt) (desktop.Scope, bool) {
	id := randomToken()[:24]
	ch := make(chan approvalReply, 1)
	c.mu.Lock()
	select {
	case <-c.closed:
		c.mu.Unlock()
		return desktop.Deny, false
	default:
		c.pending[id] = ch
	}
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	timeoutSec := int(hostApprovalWait / time.Second)
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := int(time.Until(deadline).Seconds()); remaining > 0 && remaining < timeoutSec {
			timeoutSec = remaining
		}
	}
	if err := c.send(approvalFrame{Type: "ask", ID: id, Prompt: &prompt, TimeoutSec: timeoutSec}); err != nil {
		c.close()
		return desktop.Deny, false
	}

	select {
	case reply, ok := <-ch:
		if !ok {
			return desktop.Deny, false
		}
		return reply.scope, reply.answered
	case <-ctx.Done():
		_ = c.send(approvalFrame{Type: "cancel", ID: id})
		return desktop.Deny, false
	case <-c.closed:
		return desktop.Deny, false
	}
}

func (c *remoteApprovalConn) readLoop() {
	defer c.close()
	scanner := bufio.NewScanner(c.stream)
	scanner.Buffer(make([]byte, 4096), remoteApprovalFrameLimit)
	for scanner.Scan() {
		var frame approvalFrame
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			return
		}
		switch frame.Type {
		case "pong":
		case "answer":
			scope, ok := parseApprovalScope(frame.Scope)
			if !ok {
				scope = desktop.Deny
			}
			c.mu.Lock()
			ch := c.pending[frame.ID]
			delete(c.pending, frame.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- approvalReply{scope: scope, answered: frame.Answered}
			}
		}
	}
}

func (c *remoteApprovalConn) pingLoop(ctx context.Context) {
	ticker := time.NewTicker(remoteApprovalPingEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.close()
			return
		case <-c.closed:
			return
		case <-ticker.C:
			if err := c.send(approvalFrame{Type: "ping"}); err != nil {
				c.close()
				return
			}
		}
	}
}

// remoteApprovalHub holds the currently attached desktop client approval stream
// on the remote server.
type remoteApprovalHub struct {
	mu     sync.Mutex
	active *remoteApprovalConn
}

func (h *remoteApprovalHub) attach(conn *remoteApprovalConn) {
	h.mu.Lock()
	previous := h.active
	h.active = conn
	h.mu.Unlock()
	if previous != nil && previous != conn {
		previous.close()
	}
}

func (h *remoteApprovalHub) detach(conn *remoteApprovalConn) {
	h.mu.Lock()
	if h.active == conn {
		h.active = nil
	}
	h.mu.Unlock()
}

func (h *remoteApprovalHub) connected() bool {
	h.mu.Lock()
	active := h.active
	h.mu.Unlock()
	if active == nil {
		return false
	}
	select {
	case <-active.closed:
		return false
	default:
		return true
	}
}

func (h *remoteApprovalHub) ask(ctx context.Context, prompt remoteApprovalPrompt) (desktop.Scope, bool) {
	h.mu.Lock()
	active := h.active
	h.mu.Unlock()
	if active == nil {
		return desktop.Deny, false
	}
	return active.ask(ctx, prompt)
}

func (h *remoteApprovalHub) close() {
	h.mu.Lock()
	active := h.active
	h.active = nil
	h.mu.Unlock()
	if active != nil {
		active.close()
	}
}

func parseApprovalScope(value string) (desktop.Scope, bool) {
	switch value {
	case "deny":
		return desktop.Deny, true
	case "once":
		return desktop.Once, true
	case "always":
		return desktop.Always, true
	default:
		return desktop.Deny, false
	}
}

func formatApprovalScope(scope desktop.Scope) string {
	switch scope {
	case desktop.Once:
		return "once"
	case desktop.Always:
		return "always"
	default:
		return "deny"
	}
}

// remoteApprovalChannel upgrades an SSH-authenticated desktop client request
// into a bidirectional approval stream for host/guest tool confirmations.
func (s *Server) remoteApprovalChannel(w http.ResponseWriter, r *http.Request) {
	if !s.hasRemoteDesktopView(r) {
		http.Error(w, "需要远程桌面客户端身份", http.StatusForbidden)
		return
	}
	if !httpguts.HeaderValuesContainsToken(r.Header["Connection"], "Upgrade") ||
		!strings.EqualFold(r.Header.Get("Upgrade"), remoteApprovalProtocol) {
		http.Error(w, "需要升级到审批通道", http.StatusUpgradeRequired)
		return
	}
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "无法建立审批通道", http.StatusInternalServerError)
		return
	}
	if _, err := io.WriteString(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: "+remoteApprovalProtocol+"\r\n\r\n"); err != nil {
		_ = conn.Close()
		return
	}
	if err := rw.Flush(); err != nil {
		_ = conn.Close()
		return
	}
	stream := &hijackedStream{ReadWriteCloser: conn, reader: rw.Reader}
	client := newRemoteApprovalConn(stream)
	s.remoteApprovals.attach(client)
	defer func() {
		s.remoteApprovals.detach(client)
		client.close()
	}()
	go client.pingLoop(s.lifetimeContext())
	client.readLoop()
}

type hijackedStream struct {
	io.ReadWriteCloser
	reader *bufio.Reader
}

func (h *hijackedStream) Read(p []byte) (int, error) { return h.reader.Read(p) }

// validateRemoteApprovalPrompt re-validates every string on the client side
// before displaying a native OS confirmation dialog.
func validateRemoteApprovalPrompt(p remoteApprovalPrompt) error {
	check := func(s string, maxLen int) error {
		if len(s) > maxLen {
			return errors.New("approval prompt exceeds length limit")
		}
		if strings.ContainsFunc(s, func(r rune) bool {
			return (unicode.IsControl(r) && r != '\n' && r != '\t') || unicode.Is(unicode.Cf, r)
		}) {
			return errors.New("approval prompt contains control or invisible characters")
		}
		return nil
	}
	switch p.Kind {
	case "command":
		if strings.TrimSpace(p.Command) == "" || strings.TrimSpace(p.Dir) == "" {
			return errors.New("command approval prompt is incomplete")
		}
		if err := check(p.Command, hostApprovalCommandMax); err != nil {
			return err
		}
		if err := check(p.Dir, 1024); err != nil {
			return err
		}
		return check(p.Project, 1024)
	case "tool":
		if strings.TrimSpace(p.Plugin) == "" || strings.TrimSpace(p.Tool) == "" {
			return errors.New("tool approval prompt is incomplete")
		}
		if err := check(p.Plugin, 80); err != nil {
			return err
		}
		if err := check(p.Tool, 200); err != nil {
			return err
		}
		return check(p.Arguments, hostApprovalCommandMax)
	default:
		return errors.New("unknown approval prompt kind")
	}
}

// formatRemoteApprovalDialog builds localized native dialog wording locally on
// the client, explicitly naming the remote host so a compromised remote server
// cannot spoof local-machine wording.
func formatRemoteApprovalDialog(hostName string, p remoteApprovalPrompt) (title, msg string) {
	label := strings.TrimSpace(hostName)
	if label == "" {
		label = "远程主机"
	}
	if p.Kind == "command" {
		project := filepath.Base(p.Project)
		if project == "" || project == "." || project == string(filepath.Separator) {
			project = p.Project
		}
		return fmt.Sprintf("允许在远程主机「%s」上运行命令？", label),
			"远程主机　" + label + "\n项目「" + project + "」\n目录　" + p.Dir + "\n\n" + p.Command +
				"\n\n命令将以远程主机的系统账户权限在该服务器上运行。「始终允许」会记住远程主机上对该项目的授权，以后在该项目中运行命令不再询问；可在插件页随时撤销。"
	}
	title = fmt.Sprintf("允许远程主机「%s」上的 Claude 使用插件？", label)
	source := ""
	if p.FromView {
		title = fmt.Sprintf("允许远程主机「%s」上的插件页面使用工具？", label)
		source = "\n来源　插件自己的页面"
	}
	return title, "远程主机　" + label + "\n插件「" + p.Plugin + "」\n工具　" + p.Tool + source + "\n\n" + p.Arguments +
		"\n\n插件将在远程主机上运行。「始终允许」会记住远程主机上对该插件的授权，以后不再询问；可在插件页随时撤销。"
}

// runRemoteApprovalClient attaches to a remote server's approval channel and
// displays native confirmation dialogs on this desktop computer until ctx ends.
func runRemoteApprovalClient(ctx context.Context, session *remotehost.Session, hostName string, ask func(context.Context, string, string) (desktop.Scope, bool)) {
	if ask == nil {
		ask = desktop.AskScope
	}
	backoff := 500 * time.Millisecond
	for {
		if ctx.Err() != nil || session.Context.Err() != nil {
			return
		}
		err := serveRemoteApprovalOnce(ctx, session, hostName, ask)
		if ctx.Err() != nil || session.Context.Err() != nil {
			return
		}
		if err != nil {
			diagnostics.Log("info", "host-tools", "remote approval channel ended: "+err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-session.Context.Done():
			return
		case <-time.After(backoff):
			if backoff < 5*time.Second {
				backoff *= 2
			}
		}
	}
}

func serveRemoteApprovalOnce(ctx context.Context, session *remotehost.Session, hostName string, ask func(context.Context, string, string) (desktop.Scope, bool)) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopSession := context.AfterFunc(session.Context, cancel)
	defer stopSession()

	req, err := http.NewRequestWithContext(runCtx, http.MethodPost, session.Origin+"/api/host-tools/approvals/channel", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", remoteApprovalProtocol)
	res, err := session.Transport.RoundTrip(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusLocked || res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusForbidden {
		// Remote account is locked or older server does not support approvals yet.
		return fmt.Errorf("status %d", res.StatusCode)
	}
	stream, ok := res.Body.(io.ReadWriteCloser)
	if res.StatusCode != http.StatusSwitchingProtocols || !ok || !strings.EqualFold(res.Header.Get("Upgrade"), remoteApprovalProtocol) {
		return fmt.Errorf("unexpected upgrade response %d", res.StatusCode)
	}
	stopStream := context.AfterFunc(runCtx, func() { _ = stream.Close() })
	defer stopStream()

	var writeMu sync.Mutex
	send := func(frame approvalFrame) error {
		data, err := json.Marshal(frame)
		if err != nil {
			return err
		}
		data = append(data, '\n')
		writeMu.Lock()
		defer writeMu.Unlock()
		_, err = stream.Write(data)
		return err
	}

	var dialogMu sync.Mutex
	var cancelsMu sync.Mutex
	cancels := map[string]context.CancelFunc{}
	defer func() {
		cancelsMu.Lock()
		for _, fn := range cancels {
			fn()
		}
		cancelsMu.Unlock()
	}()

	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 4096), remoteApprovalFrameLimit)
	for scanner.Scan() {
		var frame approvalFrame
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			return err
		}
		switch frame.Type {
		case "ping":
			if err := send(approvalFrame{Type: "pong"}); err != nil {
				return err
			}
		case "cancel":
			cancelsMu.Lock()
			fn := cancels[frame.ID]
			delete(cancels, frame.ID)
			cancelsMu.Unlock()
			if fn != nil {
				fn()
			}
		case "ask":
			if frame.ID == "" || frame.Prompt == nil {
				continue
			}
			prompt := *frame.Prompt
			if err := validateRemoteApprovalPrompt(prompt); err != nil {
				_ = send(approvalFrame{Type: "answer", ID: frame.ID, Scope: "deny", Answered: true})
				continue
			}
			wait := hostApprovalWait
			if frame.TimeoutSec > 0 && time.Duration(frame.TimeoutSec)*time.Second < wait {
				wait = time.Duration(frame.TimeoutSec) * time.Second
			}
			askCtx, askCancel := context.WithTimeout(runCtx, wait)
			cancelsMu.Lock()
			cancels[frame.ID] = askCancel
			cancelsMu.Unlock()

			id := frame.ID
			go func() {
				defer func() {
					askCancel()
					cancelsMu.Lock()
					delete(cancels, id)
					cancelsMu.Unlock()
				}()
				dialogMu.Lock()
				defer dialogMu.Unlock()
				if askCtx.Err() != nil {
					_ = send(approvalFrame{Type: "answer", ID: id, Scope: "deny", Answered: false})
					return
				}
				title, msg := formatRemoteApprovalDialog(hostName, prompt)
				scope, answered := ask(askCtx, title, msg)
				_ = send(approvalFrame{Type: "answer", ID: id, Scope: formatApprovalScope(scope), Answered: answered})
			}()
		}
	}
	return scanner.Err()
}
