package workbench

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/guestos"
	"github.com/sylvinus/agent-vm/internal/host"
)

var (
	errScreenSharingNotMacOS    = errors.New("仅 macOS 客机支持打开系统屏幕共享")
	errScreenSharingNotReady    = errors.New("macOS 客机尚未就绪")
	errScreenSharingNeedMacHost = errors.New("打开客机屏幕共享需要在这台 Mac 上的 Cage 应用中操作")
	errScreenSharingNotIsolated = errors.New("客机外网当前已放行或正在检查，已拒绝重新配置屏幕共享。请先在工作台暂停外网，确认网络门槛为关闭后再试。")
	errScreenSharingGateUnknown = errors.New("无法确认客机网络隔离状态，已拒绝重新配置屏幕共享。请确认环境已启动，再在外网关闭时重试。")
	errScreenSharingUnreachable = errors.New("无法连接到客机屏幕共享。请在客机「系统设置 → 通用 → 共享」中打开「屏幕共享」，并允许当前普通客机用户访问。macOS 26 仅用命令行启用时，连接可能提示「屏幕共享在 localhost 上不被允许」，需要在系统设置里再手动打开一次。")
	errScreenSharingBadAction   = errors.New("无效的屏幕共享操作")
	errScreenSharingEnable      = errors.New("无法重新启用客机屏幕共享服务。请先在工作台暂停外网，再确认客机已安装启用脚本后重试。")
)

// macOSScreenSharing is the cage-serve-owned SSH local forward to guest 5900.
// Hooks are for tests; nil uses production gate, ssh, inspect and open(1) behavior.
type macOSScreenSharing struct {
	mu      sync.Mutex
	done    <-chan struct{}
	cancel  context.CancelFunc
	port    int
	gate    func(context.Context) (cage.GateStatus, error)
	open    func(string) error
	inspect func(context.Context) (macOSScreenSharingStatus, error)
	enable  func(context.Context) error
}

type macOSScreenSharingRequest struct {
	Action string `json:"action"`
}

// screenSharingCheck mirrors the initialization / safety check item shape
// already used in the setup UI: id, title, state, evidence, remedy.
type screenSharingCheck struct {
	ID       string          `json:"id"`
	Title    string          `json:"title"`
	State    cage.CheckState `json:"state"`
	Evidence string          `json:"evidence"`
	Remedy   string          `json:"remedy,omitempty"`
}

// macOSScreenSharingStatus is the result of a Screen Sharing usability check.
type macOSScreenSharingStatus struct {
	Usable     bool                 `json:"usable"`
	Satisfied  bool                 `json:"satisfied,omitempty"`
	State      cage.CheckState      `json:"state"`
	Summary    string               `json:"summary"`
	Authorized cage.CheckState      `json:"authorized,omitempty"`
	Checks     []screenSharingCheck `json:"checks"`
}

func screenSharingIsolationError(state cage.GateStatus, err error) error {
	if err != nil {
		return errScreenSharingGateUnknown
	}
	if state.Mode != cage.GateClosed {
		return errScreenSharingNotIsolated
	}
	return nil
}

func (s *Server) requireMacOSNetworkIsolated(ctx context.Context) error {
	if s.screenSharing.gate != nil {
		state, err := s.screenSharing.gate(ctx)
		return screenSharingIsolationError(state, err)
	}
	client, err := s.gateClient()
	if err != nil {
		return screenSharingIsolationError(cage.GateStatus{}, err)
	}
	state, err := client.Status(ctx)
	if err == nil {
		s.publishGate(state)
	}
	return screenSharingIsolationError(state, err)
}

func (s *Server) openMacOSScreenSharing(w http.ResponseWriter, r *http.Request) {
	if !s.hasTrustedView(r) {
		http.Error(w, errScreenSharingNeedMacHost.Error(), http.StatusForbidden)
		return
	}
	if s.macosGuest == nil {
		requestError(w, errScreenSharingNotMacOS)
		return
	}
	var input macOSScreenSharingRequest
	if !readJSON(w, r, &input) {
		return
	}
	if input.Action == "" {
		input.Action = "open"
	}
	s.mu.Lock()
	ready, closing := s.ready, s.closing
	s.mu.Unlock()
	if !ready || closing {
		requestError(w, errScreenSharingNotReady)
		return
	}
	switch input.Action {
	case "check":
		status, err := s.inspectMacOSScreenSharing(r.Context())
		if err != nil {
			requestError(w, err)
			return
		}
		jsonReply(w, status)
	case "enable":
		if err := s.enableIsolatedMacOSScreenSharing(r.Context()); err != nil {
			requestError(w, err)
			return
		}
		status, err := s.inspectMacOSScreenSharing(r.Context())
		if err != nil {
			requestError(w, err)
			return
		}
		status.Summary = "已重新启用客机屏幕共享服务。" + status.Summary
		jsonReply(w, status)
	case "open":
		// Daily connect only creates a host-local SSH forward to guest
		// loopback. It adds no guest egress, so it is allowed regardless of
		// the current gate. Setup, repair and the System Settings pane still
		// require isolation.
		port, err := s.ensureMacOSScreenSharingTunnel(r.Context())
		if err != nil {
			requestError(w, err)
			return
		}
		url := fmt.Sprintf("vnc://127.0.0.1:%d", port)
		if err := s.openMacOSScreenSharingURL(url); err != nil {
			requestError(w, err)
			return
		}
		jsonReply(w, map[string]any{"ok": true, "address": "127.0.0.1:" + strconv.Itoa(port)})
	default:
		requestError(w, errScreenSharingBadAction)
	}
}

func (s *Server) openMacOSScreenSharingURL(url string) error {
	if s.screenSharing.open != nil {
		return s.screenSharing.open(url)
	}
	if runtime.GOOS != "darwin" {
		return errScreenSharingNeedMacHost
	}
	cmd := exec.Command("/usr/bin/open", url)
	host.HideCommandWindow(cmd)
	if err := cmd.Run(); err != nil {
		return errors.New("无法打开本机「屏幕共享」，请确认系统已安装该应用后重试。")
	}
	return nil
}

func (s *Server) enableIsolatedMacOSScreenSharing(ctx context.Context) error {
	// BeginCheck and Grant take this same lock. Keep the service change
	// inside the verified closed-gate interval.
	s.safetyOp.Lock()
	defer s.safetyOp.Unlock()
	if err := s.requireMacOSNetworkIsolated(ctx); err != nil {
		return err
	}
	if s.screenSharing.enable != nil {
		return s.screenSharing.enable(ctx)
	}
	if s.macosGuest == nil {
		return errScreenSharingNotMacOS
	}
	provision, err := guestos.MacOSProvisionUser(s.profile)
	if err != nil {
		return err
	}
	args, err := guestos.MacOSSSHArgs(s.profile, provision)
	if err != nil {
		return err
	}
	script := "sudo -S -p '' /usr/local/libexec/cage-enable-screensharing " + shellescape.Quote(s.macosGuest.User) + " < /Users/" + provision + "/password"
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, sshExecutable(), append(append([]string{}, args...), "-T", "cage-macos", "--", "exec /bin/sh -ec "+shellescape.Quote(script))...)
	cmd.Env = sshEnvironment()
	host.HideCommandWindow(cmd)
	if err := cmd.Run(); err != nil {
		return errScreenSharingEnable
	}
	return nil
}

func setupSharingSatisfied(status macOSScreenSharingStatus, confirmed bool) bool {
	return status.Usable && (status.Authorized == cage.CheckPassed || confirmed)
}

func (s *Server) evaluateMacOSSetupSharing(ctx context.Context, force bool) macOSScreenSharingStatus {
	s.mu.Lock()
	if !force && time.Since(s.macSetupSharingAttempt) < 15*time.Second && s.macSetupSharingStatus.State != "" {
		cached := s.macSetupSharingStatus
		cached.Satisfied = setupSharingSatisfied(cached, s.macSetupSharingConfirmed)
		s.mu.Unlock()
		return cached
	}
	attempt := time.Now()
	s.macSetupSharingAttempt = attempt
	s.mu.Unlock()
	status, err := s.inspectMacOSScreenSharing(ctx)
	if err != nil {
		status = macOSScreenSharingStatus{
			State:   cage.CheckUnknown,
			Summary: "暂时无法检查客机屏幕共享：" + err.Error(),
			Checks: []screenSharingCheck{{
				ID:       "availability",
				Title:    "屏幕共享可达性",
				State:    cage.CheckUnknown,
				Evidence: err.Error(),
				Remedy:   "确认虚拟 Mac 已登录后重试。若仍失败，请先暂停外网，再重新启用屏幕共享服务。",
			}},
		}
	}
	s.mu.Lock()
	status.Satisfied = setupSharingSatisfied(status, s.macSetupSharingConfirmed)
	if s.macSetupSharingAttempt == attempt {
		s.macSetupSharingStatus = status
	}
	s.mu.Unlock()
	return status
}

func (s *Server) inspectMacOSScreenSharing(ctx context.Context) (macOSScreenSharingStatus, error) {
	if s.macosGuest == nil {
		return macOSScreenSharingStatus{}, errScreenSharingNotMacOS
	}
	if s.screenSharing.inspect != nil {
		return s.screenSharing.inspect(ctx)
	}
	service, portOpen, serviceErr := s.inspectMacOSScreenSharingService(ctx)
	rfb, err := s.inspectMacOSScreenSharingRFB(ctx)
	return assembleScreenSharingStatus(service, portOpen, serviceErr, rfb, err), nil
}

type screenSharingRFBResult struct {
	Greeting    string
	Types       int
	Reason      string
	Denied      bool
	Handshake   bool
	Unreachable bool
}

func assembleScreenSharingStatus(serviceLoaded, portOpen bool, serviceErr error, rfb screenSharingRFBResult, rfbErr error) macOSScreenSharingStatus {
	service := screenSharingCheck{ID: "service", Title: "屏幕共享服务", State: cage.CheckUnknown, Evidence: "尚未取得服务状态"}
	if serviceErr != nil {
		service.Evidence = "无法确认 com.apple.screensharing 是否已加载：" + serviceErr.Error()
		service.Remedy = "请先在工作台暂停外网，再重新启用屏幕共享服务。"
		if portOpen {
			service.State = cage.CheckPassed
			service.Evidence = "未能直接读取服务状态，但客机 5900 端口已在监听，视为服务已启动。"
			service.Remedy = ""
		}
	} else if serviceLoaded {
		service.State = cage.CheckPassed
		service.Evidence = "已加载 com.apple.screensharing 服务。"
	} else {
		service.State = cage.CheckFailed
		service.Evidence = "未加载 com.apple.screensharing 服务。"
		service.Remedy = "请先在工作台暂停外网，再点击「重新启用屏幕共享服务」。首次安装会在置备阶段自动启用。"
	}

	port := screenSharingCheck{ID: "port", Title: "5900 端口监听", State: cage.CheckFailed, Evidence: "客机 127.0.0.1:5900 尚未监听。", Remedy: "请先在工作台暂停外网，再重新启用屏幕共享服务。"}
	if portOpen {
		port.State = cage.CheckPassed
		port.Evidence = "客机 127.0.0.1:5900 正在监听。"
		port.Remedy = ""
	}

	handshake := screenSharingCheck{ID: "rfb", Title: "RFB 握手", State: cage.CheckUnknown, Evidence: "尚未完成 RFB 握手。"}
	authorization := screenSharingCheck{ID: "authorization", Title: "系统设置授权", State: cage.CheckUnknown, Evidence: "尚无法自动确认系统设置中的屏幕共享开关。macOS 26 仅用命令行启用时，连接可能提示「屏幕共享在 localhost 上不被允许」。", Remedy: "请在客机「系统设置 → 通用 → 共享」中打开「屏幕共享」，并允许当前普通客机用户访问。"}
	authorized := cage.CheckUnknown
	usable := false

	if rfbErr != nil && !rfb.Handshake {
		handshake.State = cage.CheckFailed
		handshake.Evidence = "无法完成 RFB 握手：" + rfbErr.Error()
		handshake.Remedy = "请在客机「系统设置 → 通用 → 共享」中打开「屏幕共享」。若服务未启动，请先暂停外网再重新启用。"
		if rfb.Unreachable || !portOpen {
			handshake.Evidence = "尚未连接到客机屏幕共享端口。"
		}
	} else if rfb.Denied {
		handshake.State = cage.CheckFailed
		reason := rfb.Reason
		if reason == "" {
			reason = "连接在握手后被拒绝"
		}
		handshake.Evidence = "RFB 握手被拒绝：" + reason
		handshake.Remedy = "这通常表示尚未在系统设置中授权。请打开「系统设置 → 通用 → 共享」，打开「屏幕共享」，并允许当前普通客机用户访问。"
		authorization.State = cage.CheckFailed
		authorization.Evidence = "检测到屏幕共享未被允许：" + reason
		authorization.Remedy = handshake.Remedy
		authorized = cage.CheckFailed
	} else if rfb.Handshake {
		handshake.State = cage.CheckPassed
		evidence := "已收到 RFB 握手"
		if rfb.Greeting != "" {
			evidence += "（" + strings.TrimSpace(rfb.Greeting) + "）"
		}
		if rfb.Types > 0 {
			evidence += fmt.Sprintf("，服务端提供 %d 种认证方式。", rfb.Types)
		} else {
			evidence += "。"
		}
		handshake.Evidence = evidence
		usable = true
		if rfb.Reason == "" && rfb.Types > 0 {
			authorization.State = cage.CheckUnknown
			authorization.Evidence = "RFB 握手已成功，但系统完整性保护下无法直接读取「屏幕共享」开关。若本机客户端仍提示「屏幕共享在 localhost 上不被允许」，请在客机系统设置中再手动打开一次。"
		}
	}

	if !portOpen && !rfb.Handshake {
		usable = false
	}
	if authorized == cage.CheckFailed {
		usable = false
	}

	checks := []screenSharingCheck{service, port, handshake, authorization}
	state := cage.CheckFailed
	summary := "客机屏幕共享尚未可用。"
	if usable {
		state = cage.CheckPassed
		summary = "客机屏幕共享服务已启用，并且 RFB 握手成功。"
		if authorized == cage.CheckUnknown {
			summary += "请仍在「系统设置 → 通用 → 共享」中确认已打开「屏幕共享」，以免本机客户端提示「不被允许」。"
		}
	} else if authorized == cage.CheckFailed {
		summary = "屏幕共享服务已响应，但系统尚未允许连接。请在「系统设置 → 通用 → 共享」中打开「屏幕共享」。"
		state = cage.CheckFailed
	} else if service.State == cage.CheckUnknown && port.State != cage.CheckPassed && handshake.State != cage.CheckPassed {
		state = cage.CheckUnknown
		summary = "暂时无法确认客机屏幕共享状态。"
	}
	return macOSScreenSharingStatus{Usable: usable, State: state, Summary: summary, Authorized: authorized, Checks: checks}
}

func (s *Server) inspectMacOSScreenSharingService(ctx context.Context) (bool, bool, error) {
	if s.macosGuest == nil {
		return false, false, errScreenSharingNotMacOS
	}
	script := `service=no
if launchctl print system/com.apple.screensharing >/dev/null 2>&1 || launchctl list com.apple.screensharing >/dev/null 2>&1; then
	service=yes
fi
port=no
if nc -z 127.0.0.1 5900 >/dev/null 2>&1 || nc -z ::1 5900 >/dev/null 2>&1; then
	port=yes
fi
printf 'service=%s port=%s\n' "$service" "$port"`
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := s.macOSSSHCommand(ctx, "-T", "cage-macos", "--", "exec /bin/sh -c "+shellescape.Quote(script))
	out, err := cmd.Output()
	line := strings.TrimSpace(string(out))
	service := strings.Contains(line, "service=yes")
	port := strings.Contains(line, "port=yes")
	if err != nil && !service && !port {
		return false, false, err
	}
	return service, port, nil
}

func (s *Server) inspectMacOSScreenSharingRFB(ctx context.Context) (screenSharingRFBResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := s.macOSSSHCommand(ctx, "-W", "127.0.0.1:5900", "cage-macos")
	if cmd.Err != nil {
		return screenSharingRFBResult{Unreachable: true}, cmd.Err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return screenSharingRFBResult{}, err
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return screenSharingRFBResult{}, err
	}
	defer stdout.Close()
	if err := cmd.Start(); err != nil {
		return screenSharingRFBResult{Unreachable: true}, err
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	return readScreenSharingRFB(ctx, struct {
		io.Reader
		io.Writer
	}{stdout, stdin})
}

func probeScreenSharingRFB(ctx context.Context, addr string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return errScreenSharingUnreachable
	}
	defer conn.Close()
	result, err := readScreenSharingRFB(ctx, conn)
	if err != nil || result.Denied || !result.Handshake {
		return errScreenSharingUnreachable
	}
	return nil
}

func readScreenSharingRFB(ctx context.Context, conn io.ReadWriter) (screenSharingRFBResult, error) {
	var result screenSharingRFBResult
	if deadline, ok := ctx.Deadline(); ok {
		if c, ok := conn.(interface{ SetDeadline(time.Time) error }); ok {
			_ = c.SetDeadline(deadline)
		}
	} else if c, ok := conn.(interface{ SetDeadline(time.Time) error }); ok {
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	}
	var buf [12]byte
	if _, err := io.ReadFull(conn, buf[:]); err != nil || !strings.HasPrefix(string(buf[:]), "RFB 003.") {
		return result, errScreenSharingUnreachable
	}
	result.Greeting = strings.TrimRight(string(buf[:]), "\n")
	version := string(buf[:])
	switch version {
	case "RFB 003.003\n", "RFB 003.007\n", "RFB 003.008\n":
	case "RFB 003.889\n": // Apple Remote Desktop also supports RFB 3.8.
		version = "RFB 003.008\n"
	default:
		return result, errScreenSharingUnreachable
	}
	if _, err := io.WriteString(conn, version); err != nil {
		return result, errScreenSharingUnreachable
	}
	reader := bufio.NewReader(conn)
	var count byte
	if version == "RFB 003.003\n" {
		var securityType uint32
		if err := binary.Read(reader, binary.BigEndian, &securityType); err != nil {
			return result, errScreenSharingUnreachable
		}
		if securityType != 0 {
			result.Handshake = true
			result.Types = 1
			return result, nil
		}
	} else {
		var err error
		count, err = reader.ReadByte()
		if err != nil {
			result.Handshake = true
			result.Denied = true
			result.Reason = "版本交换后连接已关闭，可能尚未在系统设置中授权"
			return result, nil
		}
	}
	if count == 0 {
		var length uint32
		if err := binary.Read(reader, binary.BigEndian, &length); err != nil {
			result.Handshake = true
			result.Denied = true
			result.Reason = "服务器拒绝连接且未返回原因"
			return result, nil
		}
		if length > 2048 {
			length = 2048
		}
		reason := make([]byte, length)
		_, _ = io.ReadFull(reader, reason)
		result.Handshake = true
		result.Reason = strings.TrimSpace(string(reason))
		// A zero security-type count is always a refusal in RFB 3.8.
		result.Denied = true
		return result, nil
	}
	if _, err := io.CopyN(io.Discard, reader, int64(count)); err != nil {
		return result, errScreenSharingUnreachable
	}
	result.Handshake = true
	result.Types = int(count)
	return result, nil
}

func (s *Server) ensureMacOSScreenSharingTunnel(ctx context.Context) (int, error) {
	session := &s.screenSharing
	session.mu.Lock()
	defer session.mu.Unlock()
	s.mu.Lock()
	ready := s.ready && !s.closing
	life := s.lifetimeContextLocked()
	s.mu.Unlock()
	if !ready {
		return 0, errScreenSharingNotReady
	}
	deadline, cancelWait := context.WithTimeout(ctx, 8*time.Second)
	defer cancelWait()
	if session.aliveLocked() {
		if err := probeScreenSharingRFB(deadline, net.JoinHostPort("127.0.0.1", strconv.Itoa(session.port))); err == nil && session.aliveLocked() {
			return session.port, nil
		}
	}
	session.stopLocked()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, errors.New("无法在本机环回地址上分配屏幕共享端口")
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		return 0, err
	}
	args, err := guestos.MacOSScreenSharingSSHArgs(s.profile, s.macosGuest.User, port)
	if err != nil {
		return 0, err
	}
	args = append(args, "cage-macos")
	cmdCtx, cancel := context.WithCancel(life)
	cmd := exec.CommandContext(cmdCtx, sshExecutable(), args...)
	cmd.Env = sshEnvironment()
	host.HideCommandWindow(cmd)
	if cmd.Err != nil {
		cancel()
		return 0, errors.New("无法准备客机 SSH 转发")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return 0, errors.New("无法启动客机 SSH 转发")
	}
	// OpenSSH emits the marker from its fixed LocalCommand only after it
	// authenticates the guest and binds the requested port. A different
	// listener can otherwise win the port allocation race and imitate RFB.
	bound := make(chan bool, 1)
	done := make(chan struct{})
	go func() {
		var marker [1]byte
		_, err := io.ReadFull(stdout, marker[:])
		bound <- err == nil && marker[0] == '1'
		_ = cmd.Wait()
		close(done)
	}()
	session.cancel, session.done, session.port = cancel, done, port
	select {
	case ok := <-bound:
		if ok && probeScreenSharingRFB(deadline, net.JoinHostPort("127.0.0.1", strconv.Itoa(port))) == nil && session.aliveLocked() {
			return port, nil
		}
	case <-deadline.Done():
	case <-life.Done():
	}
	session.stopLocked()
	return 0, errScreenSharingUnreachable
}

func (ss *macOSScreenSharing) aliveLocked() bool {
	if ss.done == nil {
		return false
	}
	select {
	case <-ss.done:
		return false
	default:
		return true
	}
}

func (s *Server) closeMacOSScreenSharing() {
	s.screenSharing.mu.Lock()
	defer s.screenSharing.mu.Unlock()
	s.screenSharing.stopLocked()
}

func (ss *macOSScreenSharing) stopLocked() {
	if ss.cancel != nil {
		ss.cancel()
		<-ss.done
		ss.cancel = nil
		ss.done = nil
		ss.port = 0
	}
}
