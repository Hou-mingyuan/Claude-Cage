package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/cagecmd"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
	"golang.org/x/term"
)

type hostCLIStatus struct {
	Ready         bool  `json:"ready"`
	Running       bool  `json:"vm_running"`
	NetworkOpen   *bool `json:"network_open"`
	NetworkPaused bool  `json:"network_paused"`
	Job           Job   `json:"job"`
}

const cliServiceRunning = "Cage 后台服务正在运行。"
const cliStatusPath = "/api/status?view=runtime"

func cliCall(ctx context.Context, c *controllerClient, method, path string, in, out any) error {
	code, reason, err := c.callReason(ctx, method, path, in, out)
	if err != nil {
		return err
	}
	if code == http.StatusOK {
		return nil
	}
	if reason == "" {
		reason = http.StatusText(code)
	}
	return &AgentError{HTTPStatus: code, Message: reason}
}

func waitCLIJob(ctx context.Context, c *controllerClient, job Job, allowOffline bool) error {
	if job.ID == "" {
		return errors.New("后台没有返回环境任务编号")
	}
	for job.Running {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
		var next Job
		if err := cliCall(ctx, c, "GET", "/api/agent/jobs/"+job.ID, nil, &next); err != nil {
			return err
		}
		if next.ID != job.ID {
			return errors.New("后台任务已被替换，请检查环境状态")
		}
		job = next
	}
	if job.Canceled || job.SupersededBy != "" {
		return errors.New("环境任务已取消或被替换")
	}
	if job.Error != "" && !(allowOffline && job.NetworkCheckFailed) {
		return errors.New(job.Error)
	}
	return nil
}

func runCLILifecycle(ctx context.Context, c *controllerClient, action string, allowOffline bool) error {
	var job Job
	path := "/api/" + action
	var input any = struct{}{}
	if action == "rebuild" {
		path = "/api/start"
		input = map[string]string{"reset_disk_token": hostCLIProof(c.record.Token)}
	}
	if err := cliCall(ctx, c, "POST", path, input, &job); err != nil {
		var e *AgentError
		if !errors.As(err, &e) || e.HTTPStatus != 409 {
			return err
		}
		var status hostCLIStatus
		if cliCall(ctx, c, "GET", cliStatusPath, nil, &status) != nil || !status.Job.Running || status.Job.Kind != action {
			return err
		}
		job = status.Job
	}
	return waitCLIJob(ctx, c, job, allowOffline)
}

func launchHostCLIDesktop(config string, windowOnly bool) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	dir := filepath.Dir(exe)
	windowSetting := "0"
	if windowOnly {
		windowSetting = "1"
	}
	launchEnv := []string{cage.ConfigEnv + "=" + config, desktopWindowOnlyEnv + "=" + windowSetting}
	var command string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		bundle := filepath.Dir(filepath.Dir(dir))
		if filepath.Ext(bundle) != ".app" {
			return errors.New("请使用 Cage.app 自带的终端命令打开工作台")
		}
		command = "/usr/bin/open"
		args = []string{"-a", bundle}
		for _, entry := range launchEnv {
			args = append(args, "--env", entry)
		}
	case "windows":
		command = filepath.Join(filepath.Dir(dir), "Cage.exe")
	default:
		command = filepath.Join(dir, "cage-desktop")
		if _, err = os.Stat(command); err != nil {
			command = "/usr/bin/cage-desktop"
		}
	}
	if _, err = os.Stat(command); err != nil {
		return errors.New("无法找到随 Cage 安装的桌面窗口")
	}
	return startDetached(windowVisible, func() *exec.Cmd {
		cmd := exec.Command(command, args...)
		cmd.Env = append(os.Environ(), launchEnv...)
		return cmd
	})
}

// A freshly opened desktop may still be on its environment chooser. Select
// the local workload with the same generation checks as the native window.
func showHostCLIDesktop(ctx context.Context, config string, workload *controllerClient) error {
	if err := launchHostCLIDesktop(config, false); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	var desktop *controllerClient
	defer func() {
		if desktop != nil {
			desktop.close()
		}
	}()
	for {
		if desktop == nil {
			desktop, _ = agentConnectTarget(ctx, config, AgentTarget{Scope: "desktop"})
		}
		if desktop != nil {
			var view ClientContext
			if err := cliCall(ctx, desktop, "GET", "/api/client/context", nil, &view); err != nil {
				return err
			}
			if view.Mode == "remote" || view.Candidate != nil && view.Candidate.Mode != "local" {
				return errors.New("Cage 窗口正在使用远程环境。请切换到本机环境后重试此命令。")
			}
			if view.Mode == "local" && view.State.State == "connected" {
				// A hidden window may still point at the controller replaced by
				// restart. Probe its selected generation before showing login.
				selected := *desktop
				selected.connectionID = view.ConnectionID
				code, err := selected.callContext(ctx, "GET", "/api/account", nil, nil)
				if err != nil {
					return err
				}
				if code == http.StatusOK {
					next, err := connectHostCage(ctx, config, false)
					if err != nil {
						return err
					}
					workload.close()
					*workload = *next
					return nil
				}
				if code != http.StatusConflict && code != http.StatusBadGateway && code != http.StatusServiceUnavailable {
					return fmt.Errorf("无法确认 Cage 窗口的本机连接（HTTP %d）", code)
				}
			}
			input := switchRequest{Mode: "local", ConnectionID: view.ConnectionID}
			path := ""
			if view.Candidate == nil {
				path = "candidate"
			} else {
				switch view.Candidate.Connection.State {
				case "connected":
					input.CandidateID = view.Candidate.ID
					input.LocalAction = "auto"
					path = "commit"
				case "failed":
					return fmt.Errorf("无法连接 Cage 本机窗口：%s", view.Candidate.Connection.Message)
				}
			}
			if path != "" {
				code, reason, err := desktop.callReason(ctx, "POST", "/api/client/"+path, input, nil)
				if err != nil {
					return err
				}
				// A concurrent window may have advanced the same generation.
				if code != http.StatusOK && code != http.StatusAccepted && code != http.StatusConflict {
					return fmt.Errorf("无法打开 Cage 本机环境（HTTP %d）：%s", code, reason)
				}
			}
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return errors.New("Cage 本机窗口未就绪。请在 Cage 中打开本机环境后重试。")
			}
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func cliWorkbenchAccessHint(ctx context.Context, c *controllerClient) (string, error) {
	var account accountView
	if err := cliCall(ctx, c, "GET", "/api/account", nil, &account); err != nil {
		return "", err
	}
	if account.Exists {
		return "Cage 本机工作台已锁定，请在 Cage 窗口解锁。", nil
	}
	return "请在 Cage 窗口完成本机工作台的首次设置。", nil
}

// Resume only a trust grant the user already enabled. The same service that
// verifies the private local credential also enforces an explicit manual lock.
func cliResumeTrusted(ctx context.Context, c *controllerClient) bool {
	var account accountView
	if cliCall(ctx, c, "GET", "/api/account", nil, &account) != nil || account.RequireAuthentication || (!account.TrustedLocal && !account.TrustedLogin) {
		return false
	}
	local, err := c.localControl(ctx, "status", desktopLocalRequest{})
	if err != nil {
		return false
	}
	var reply trustedLoginReply
	in := trustedLoginRequest{Action: "resume", Token: c.record.Token, InstanceID: local.InstanceID}
	return cliCall(ctx, c, "POST", "/api/desktop-local/trusted-login", in, &reply) == nil && reply.Error == ""
}

func cliWaitAccess(ctx context.Context, c *controllerClient, config string, interactive, needsEnvironment bool, stderr io.Writer) (status hostCLIStatus, err error) {
	shown, triedTrust := false, false
	for {
		err = cliCall(ctx, c, "GET", cliStatusPath, nil, &status)
		var hint string
		if err != nil {
			var access *AgentError
			if !errors.As(err, &access) || access.HTTPStatus != http.StatusLocked {
				return status, err
			}
			if !triedTrust {
				triedTrust = true
				if cliResumeTrusted(ctx, c) {
					continue
				}
			}
			if !shown {
				if hint, err = cliWorkbenchAccessHint(ctx, c); err != nil {
					return status, err
				}
			}
		} else {
			if status.Ready || !needsEnvironment {
				return status, nil
			}
			var init initializationView
			if err = cliCall(ctx, c, "GET", "/api/environment/initialization", nil, &init); err != nil {
				return status, err
			}
			if !init.Required {
				return status, nil
			}
			hint = "请在 Cage 窗口完成本机环境的首次配置。"
		}
		if !interactive {
			return status, errors.New(hint)
		}
		if !shown {
			fmt.Fprintln(stderr, hint+"完成后，当前命令会自动继续。")
			if err = showHostCLIDesktop(ctx, config, c); err != nil {
				return status, err
			}
			shown = true
		}
		select {
		case <-ctx.Done():
			return status, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func connectHostCage(ctx context.Context, config string, ensure bool) (*controllerClient, error) {
	if ensure {
		start, cancel := context.WithTimeout(ctx, 40*time.Second)
		err := AgentStartService(start, config)
		cancel()
		if err != nil {
			return nil, err
		}
	}
	c, err := agentConnect(ctx, config)
	if err == nil {
		c.connectionID = "local"
		c.client.Timeout = 2 * time.Minute
	}
	return c, err
}

func stopHostCage(ctx context.Context, config string, interactive bool, stderr io.Writer) error {
	profile := filepath.Dir(config)
	free, err := controllerLockStatus(profile)
	if err != nil {
		return err
	}
	if free {
		running, err := controllerVMState(profile)
		if err != nil {
			return err
		}
		if !running {
			return managedproxy.Stop(profile)
		}
	}
	// An orphaned VM needs its workload controller to close the network gate,
	// stop tasks and unmount projects through the same path as the workbench.
	c, err := connectHostCage(ctx, config, true)
	if err != nil {
		return err
	}
	defer c.close()
	view, err := c.localControl(ctx, "status", desktopLocalRequest{})
	if err != nil {
		return err
	}
	if !view.Known {
		return errors.New("无法确认本机环境的状态，请在 Cage 工作台检查后重试")
	}
	if view.Busy || view.Running || view.LiveTerminals > 0 {
		if _, err = cliWaitAccess(ctx, c, config, interactive, false, stderr); err != nil {
			return err
		}
		if err = runCLILifecycle(ctx, c, "stop", false); err != nil {
			return err
		}
		view, err = c.localControl(ctx, "status", desktopLocalRequest{})
		if err != nil {
			return err
		}
	}
	if !view.Known || view.Busy || view.Running || view.LiveTerminals != 0 {
		return errors.New("本机仍有任务或虚拟机在运行，请查看停止结果")
	}
	return c.releaseLocal(ctx, view.DesktopLocalStatus, false)
}

// HostCLI is the entry point of the installed local command. Management
// requests never create a project and the execution socket is pinned locally.
func HostCLI(ctx context.Context, config, action string, args []string, in, out, errOut *os.File) (int, error) {
	if action == "up" {
		if err := launchHostCLIDesktop(config, true); err != nil {
			return 1, err
		}
		return 0, nil
	}
	if action == "stop" || action == "restart" {
		if err := stopHostCage(ctx, config, term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd())), errOut); err != nil {
			return 1, err
		}
		if action == "stop" {
			fmt.Fprintln(out, "Cage 环境和后台服务已停止。")
			return 0, nil
		}
		action = "start"
	}
	rebuild := (action == "start" || action == "login") && slices.Contains(args, "--reset")
	var selector, name string
	if action == "run-by-name" {
		if len(args) == 0 || !cagecmd.ValidAccountName(args[0]) {
			return 2, errors.New("用法：cage <账号名称> [Claude 参数]")
		}
		name, args = args[0], args[1:]
	}
	if action == "account" {
		switch {
		case len(args) == 1 && args[0] == "list":
			action = "accounts"
		case len(args) == 2 && args[0] == "use":
			action, selector = "use-account", args[1]
		case len(args) == 3 && args[0] == "rename":
			action, selector, name = "rename-account", args[1], args[2]
		case len(args) >= 2 && args[0] == "run" && strings.TrimSpace(args[1]) != "":
			action, selector, args = "run", args[1], args[2:]
			if len(args) > 0 && args[0] == "--" {
				args = args[1:]
			}
		default:
			return 2, errors.New("用法：cage account list | use <账号或 auto> | run <账号> [--] [Claude 参数] | rename <账号> <新名称>")
		}
	}
	profile := filepath.Dir(config)
	if action == "status" && controllerLockFree(profile) {
		running, err := controllerVMState(profile)
		if err != nil {
			return 1, err
		}
		fmt.Fprintln(out, "Cage 后台服务未运行。")
		if running {
			fmt.Fprintln(out, "Cage 环境正在运行。")
		} else {
			fmt.Fprintln(out, "Cage 环境已停止。")
		}
		return 0, nil
	}
	c, err := connectHostCage(ctx, config, action != "status")
	if err != nil {
		return 1, err
	}
	defer c.close()
	if action == "status" {
		fmt.Fprintln(out, cliServiceRunning)
	}
	if action == "accounts" || action == "use-account" || action == "rename-account" {
		return cliExchange(ctx, c, hostCLIRequest{Action: action, Account: selector, Name: name}, in, out, errOut, out)
	}
	var status hostCLIStatus
	if action == "status" {
		if err = cliCall(ctx, c, "GET", cliStatusPath, nil, &status); err != nil {
			var access *AgentError
			if errors.As(err, &access) && access.HTTPStatus == http.StatusLocked {
				hint, err := cliWorkbenchAccessHint(ctx, c)
				if err != nil {
					return 1, err
				}
				local, err := c.localControl(ctx, "status", desktopLocalRequest{})
				if err != nil {
					return 1, err
				}
				if local.Known && local.Running {
					fmt.Fprintln(out, "Cage 环境正在运行。")
				} else if local.Known {
					fmt.Fprintln(out, "Cage 环境已停止。")
				}
				fmt.Fprintln(out, hint)
				return 0, nil
			}
			return 1, err
		}
		if status.Ready && status.Running {
			fmt.Fprintln(out, "Cage 环境已就绪。")
			return cliExchange(ctx, c, hostCLIRequest{Action: "account-status"}, in, out, errOut, out)
		} else if status.Running {
			fmt.Fprintln(out, "Cage 环境正在准备。")
		} else {
			fmt.Fprintln(out, "Cage 环境已停止。")
		}
		return 0, nil
	}
	_, printMode := cliOption(args, "-p", "--print")
	inputTTY := term.IsTerminal(int(in.Fd()))
	tty := inputTTY && term.IsTerminal(int(out.Fd())) && !printMode && !cliInfo(args)
	var stdin io.Reader = in
	if inputTTY && !tty {
		// A native --print invocation ignores an interactive stdin. Giving the
		// guest an open pipe here would make a prompt wait forever for Ctrl+D.
		stdin = strings.NewReader("")
	}
	canOpen := action == "login" || tty
	if action == "login" {
		fmt.Fprintln(errOut, "正在准备 Claude 账号授权…")
	}
	status, err = cliWaitAccess(ctx, c, config, canOpen, action != "run-by-name", errOut)
	if err != nil {
		return 1, err
	}
	if action == "run-by-name" && (!status.Ready || status.Job.Running && (status.Job.Kind == "start" || status.Job.Kind == "rebuild")) {
		// Resolve before a cold start or a wait. Pin the same ID at admission so
		// reassigning a name during boot cannot send the prompt to another account.
		var resolved strings.Builder
		code, err := cliExchange(ctx, c, hostCLIRequest{Action: "resolve-name", Name: name}, strings.NewReader(""), &resolved, errOut, nil)
		if err != nil || code != 0 {
			return code, err
		}
		selector = resolved.String()
		if !validClaudeID(selector) {
			return 1, errors.New("后台未能确认这个名称对应的账号")
		}
		if !status.Ready {
			status, err = cliWaitAccess(ctx, c, config, canOpen, true, errOut)
			if err != nil {
				return 1, err
			}
		}
	}
	if status.Job.Running && (status.Job.Kind == "start" || status.Job.Kind == "rebuild") {
		if status.Job.Kind == "rebuild" {
			rebuild = false // This invocation is joining the rebuild already underway.
		}
		if err := waitCLIJob(ctx, c, status.Job, action == "start"); err != nil {
			return 1, err
		}
		if err := cliCall(ctx, c, "GET", cliStatusPath, nil, &status); err != nil {
			return 1, err
		}
	}
	if !status.Ready || action == "start" && !status.Running || rebuild {
		fmt.Fprintln(errOut, "正在启动 Cage 环境…")
		operation := "start"
		if rebuild {
			operation = "rebuild"
		}
		if err = runCLILifecycle(ctx, c, operation, action == "start"); err != nil {
			return 1, err
		}
		if err = cliCall(ctx, c, "GET", cliStatusPath, nil, &status); err != nil {
			return 1, err
		}
		if !status.Ready || action == "start" && !status.Running {
			return 1, errors.New("环境尚未就绪，请在 Cage 工作台查看启动详情")
		}
	}
	if action == "start" {
		fmt.Fprintln(out, "Cage 后台服务和本机环境已就绪。")
		if status.NetworkOpen != nil && !*status.NetworkOpen {
			fmt.Fprintln(errOut, "本机环境可用，外网尚未放行。请在工作台查看联网检查。")
		}
		return 0, nil
	}
	if status.Running && status.NetworkOpen != nil && !*status.NetworkOpen && (action == "login" || !cliGlobal(args) && !cliSubcommand(args)) {
		if status.NetworkPaused {
			fmt.Fprintln(errOut, "Cage 已暂停联网，可在工作台恢复。")
		} else {
			fmt.Fprintln(errOut, "Cage 外网尚未放行，请在工作台查看联网检查。")
		}
	}
	var dir string
	if (action == "run" || action == "run-by-name") && !cliGlobal(args) {
		dir, err = os.Getwd()
		if err != nil {
			return 1, err
		}
	}
	request := hostCLIRequest{Action: action, Account: selector, Name: name, Args: args, Directory: dir, TTY: tty}
	if tty {
		cols, rows, _ := term.GetSize(int(out.Fd()))
		request.Cols, request.Rows = uint16(cols), uint16(rows)
	}
	login := func() (int, error) {
		if err := showHostCLIDesktop(ctx, config, c); err != nil {
			return 1, err
		}
		return cliExchange(ctx, c, hostCLIRequest{Action: "login"}, in, out, errOut, nil)
	}
	if action == "login" {
		return login()
	}
	code, err := cliExchange(ctx, c, request, stdin, out, errOut, out)
	if errors.Is(err, errHostCLILogin) && tty && action == "run" && selector == "" {
		if loginCode, loginErr := login(); loginErr != nil || loginCode != 0 {
			return loginCode, loginErr
		}
		return cliExchange(ctx, c, request, stdin, out, errOut, out)
	}
	return code, err
}

func cliExchange(ctx context.Context, controller *controllerClient, in hostCLIRequest, stdin io.Reader, stdout, stderr io.Writer, console *os.File) (int, error) {
	u, _ := url.Parse(controller.origin)
	header := http.Header{"Origin": {controller.origin}}
	header.Set(connectionHeader, "local")
	header.Set(hostCLIHeader, hostCLIProof(controller.record.Token))
	for _, cookie := range controller.client.Jar.Cookies(u) {
		header.Add("Cookie", cookie.String())
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, resp, err := dialer.DialContext(ctx, "ws"+strings.TrimPrefix(controller.origin, "http")+"/api/host-cli", header)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
			return 1, fmt.Errorf("Cage 终端连接失败（HTTP %d）", resp.StatusCode)
		}
		return 1, err
	}
	defer ws.Close()
	ws.SetReadLimit(2 << 20)
	c := &hostCLIConn{Conn: ws}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { ws.Close() })
	defer stop()
	if err = ws.WriteJSON(in); err != nil {
		return 1, err
	}
	var restore func()
	var inputDone chan struct{}
	defer func() {
		cancel()
		_ = ws.Close()
		if inputDone != nil {
			if _, file := stdin.(*os.File); !file {
				if closer, ok := stdin.(io.ReadCloser); ok {
					_ = closer.Close()
				}
			}
			<-inputDone
		}
		if restore != nil {
			restore()
		}
	}()
	var display, errDisplay cliScreen
	filterOut := console != nil && term.IsTerminal(int(console.Fd()))
	errFile, errIsFile := stderr.(*os.File)
	filterErr := errIsFile && term.IsTerminal(int(errFile.Fd()))
	inputStarted := false
	inputAck := make(chan struct{}, 1)
	for {
		kind, data, err := ws.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return 1, ctx.Err()
			}
			return 1, errors.New("Cage 终端连接已断开，或工作台已锁定")
		}
		if kind == websocket.BinaryMessage {
			if len(data) == 0 {
				return 1, errors.New("无效的 Cage 终端数据")
			}
			dst := stdout
			chunk := data[1:]
			switch data[0] {
			case 'e':
				dst = stderr
				if filterErr {
					chunk = errDisplay.filter(chunk, false)
				}
			case 'o', 'h':
				if filterOut {
					chunk = display.filter(chunk, data[0] == 'h')
				}
			default:
				return 1, errors.New("未知的 Cage 输出通道")
			}
			if _, err = dst.Write(chunk); err != nil {
				return 1, err
			}
			continue
		}
		var e hostCLIEvent
		if err = json.Unmarshal(data, &e); err != nil {
			return 1, err
		}
		e.Message = HostCLIDiagnostic(e.Message)
		switch e.Type {
		case "exit":
			if e.Message != "" {
				fmt.Fprintln(stderr, e.Message)
			}
			return e.Code, nil
		case "error":
			return 1, errors.New(e.Message)
		case "login-required":
			return 1, errHostCLILogin
		case "notice":
			fmt.Fprintln(stderr, e.Message)
		case "input-ack":
			select {
			case inputAck <- struct{}{}:
			default:
				return 1, errors.New("重复的终端输入确认")
			}
		case "ready", "terminal":
			if inputStarted {
				return 1, errors.New("重复的终端启动响应")
			}
			inputStarted = true
			if e.Type == "terminal" && console != nil {
				if file, ok := stdin.(*os.File); ok {
					restore, err = rawCLIConsole(file, console)
					if err != nil {
						return 1, err
					}
					go cliWatchSize(ctx, c, console, in.Cols, in.Rows)
				}
			}
			inputDone = make(chan struct{})
			pipe := e.Type == "ready"
			go func() {
				defer close(inputDone)
				err := pumpCLIInput(ctx, stdin, func(data []byte) error {
					if err := c.send(websocket.BinaryMessage, data); err != nil || !pipe {
						return err
					}
					select {
					case <-inputAck:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
				if ctx.Err() != nil {
					return
				}
				if err == nil {
					_ = c.event(hostCLIEvent{Type: "eof"})
				} else {
					_ = ws.Close()
				}
			}()
		}
	}
}

func copyCLIInput(ctx context.Context, read func([]byte) (int, error), send func([]byte) error) error {
	buf := make([]byte, 32<<10)
	for ctx.Err() == nil {
		n, err := read(buf)
		if n > 0 {
			if writeErr := send(buf[:n]); writeErr != nil {
				return writeErr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

func cliWatchSize(ctx context.Context, c *hostCLIConn, f *os.File, cols, rows uint16) {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			w, h, err := term.GetSize(int(f.Fd()))
			if err == nil && (uint16(w) != cols || uint16(h) != rows) {
				cols, rows = uint16(w), uint16(h)
				if c.event(hostCLIEvent{Type: "resize", Cols: cols, Rows: rows}) != nil {
					return
				}
			}
		}
	}
}

func restoreCLIDisplay(out *os.File) {
	_, _ = out.WriteString("\x1b[?2026l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1004l\x1b[?1006l\x1b[?2004l\x1b[?1049l\x1b[?25h\x1b[0m")
}
