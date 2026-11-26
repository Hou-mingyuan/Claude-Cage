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
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
	"golang.org/x/term"
)

type hostCLIStatus struct {
	Ready         bool
	Running       bool  `json:"vm_running"`
	NetworkOpen   *bool `json:"network_open"`
	NetworkPaused bool  `json:"network_paused"`
	Job           Job
}

const cliServiceRunning = "Cage 后台服务正在运行。"

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

func waitCLIJob(ctx context.Context, c *controllerClient, job Job) error {
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
	if job.Error != "" {
		return errors.New(job.Error)
	}
	if job.Canceled || job.SupersededBy != "" {
		return errors.New("环境任务已取消或被替换")
	}
	return nil
}

func runCLILifecycle(ctx context.Context, c *controllerClient, action string) error {
	var job Job
	if err := cliCall(ctx, c, "POST", "/api/"+action, struct{}{}, &job); err != nil {
		var e *AgentError
		if !errors.As(err, &e) || e.HTTPStatus != 409 {
			return err
		}
		var status hostCLIStatus
		if cliCall(ctx, c, "GET", "/api/status", nil, &status) != nil || !status.Job.Running || status.Job.Kind != action {
			return err
		}
		job = status.Job
	}
	return waitCLIJob(ctx, c, job)
}

func launchHostCLIDesktop(config string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	dir := filepath.Dir(exe)
	var command string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		bundle := filepath.Dir(filepath.Dir(dir))
		if filepath.Ext(bundle) != ".app" {
			return errors.New("请使用 Cage.app 内的终端命令以打开登录窗口")
		}
		command = "/usr/bin/open"
		args = []string{"-a", bundle, "--env", cage.ConfigEnv + "=" + config}
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
	return startDetached(func() *exec.Cmd {
		cmd := exec.Command(command, args...)
		cmd.Env = append(os.Environ(), cage.ConfigEnv+"="+config)
		return cmd
	})
}

// A freshly opened desktop may still be on its environment chooser. Select
// the local workload with the same generation checks as the native window.
func showHostCLIDesktop(ctx context.Context, config string, workload *controllerClient) error {
	if err := launchHostCLIDesktop(config); err != nil {
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
			if view.Mode == "local" && view.State.State == "connected" {
				// The desktop can adopt an idle headless controller by restarting
				// it. Authenticate to the workload now selected by that window.
				next, err := agentConnect(ctx, config)
				if err != nil {
					return err
				}
				next.connectionID = "local"
				next.client.Timeout = workload.client.Timeout
				workload.close()
				*workload = *next
				return nil
			}
			if view.Mode == "remote" || view.Candidate != nil && view.Candidate.Mode != "local" {
				return errors.New("Cage 窗口正在使用远程环境。请切换到本机环境后重试此命令。")
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

func cliWaitAccess(ctx context.Context, c *controllerClient, config string, interactive bool, stderr io.Writer) (status hostCLIStatus, err error) {
	shown := false
	for {
		err = cliCall(ctx, c, "GET", "/api/status", nil, &status)
		var hint string
		if err != nil {
			var access *AgentError
			if !errors.As(err, &access) || access.HTTPStatus != http.StatusLocked {
				return status, err
			}
			if !shown {
				if hint, err = cliWorkbenchAccessHint(ctx, c); err != nil {
					return status, err
				}
			}
		} else {
			if status.Ready {
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

// HostCLI is the entry point of the installed local command. Management
// requests never create a project and the execution socket is pinned locally.
func HostCLI(ctx context.Context, config, action string, args []string, in, out, errOut *os.File) (int, error) {
	var selector string
	if action == "account" {
		switch {
		case len(args) == 1 && args[0] == "list":
			action = "accounts"
		case len(args) == 2 && args[0] == "use":
			action, selector = "use-account", args[1]
		case len(args) >= 2 && args[0] == "run":
			action, selector, args = "run", args[1], args[2:]
			if len(args) > 0 && args[0] == "--" {
				args = args[1:]
			}
		default:
			return 2, errors.New("用法：cage account list | use <编号或 auto> | run <编号> [--] [Claude 参数]")
		}
	}
	profile := filepath.Dir(config)
	if (action == "stop" || action == "status") && controllerLockFree(profile) {
		running, err := controllerVMState(profile)
		if err != nil {
			return 1, err
		}
		if !running || action == "status" {
			if action == "status" {
				fmt.Fprintln(out, "Cage 后台服务未运行。")
			}
			if !running && action == "stop" {
				if err = managedproxy.Stop(profile); err != nil {
					return 1, err
				}
			}
			if running {
				fmt.Fprintln(out, "Cage 环境正在运行。")
			} else {
				fmt.Fprintln(out, "Cage 环境已停止。")
			}
			return 0, nil
		}
	}
	if action != "status" {
		start, cancel := context.WithTimeout(ctx, 40*time.Second)
		err := AgentStartService(start, config)
		cancel()
		if err != nil {
			return 1, err
		}
		if action == "start" {
			fmt.Fprintln(out, cliServiceRunning)
			return 0, nil
		}
	}
	c, err := agentConnect(ctx, config)
	if err != nil {
		return 1, err
	}
	defer c.close()
	c.connectionID = "local"
	c.client.Timeout = 2 * time.Minute
	if action == "status" {
		fmt.Fprintln(out, cliServiceRunning)
	}
	if action == "accounts" || action == "use-account" {
		return cliExchange(ctx, c, hostCLIRequest{Action: action, Account: selector}, in, out, errOut, out)
	}
	var status hostCLIStatus
	if action == "status" || action == "stop" || action == "restart" {
		if err = cliCall(ctx, c, "GET", "/api/status", nil, &status); err != nil {
			var access *AgentError
			if action == "status" && errors.As(err, &access) && access.HTTPStatus == http.StatusLocked {
				hint, err := cliWorkbenchAccessHint(ctx, c)
				if err != nil {
					return 1, err
				}
				fmt.Fprintln(out, hint)
				return 0, nil
			}
			return 1, err
		}
		if action == "status" {
			if status.Ready {
				fmt.Fprintln(out, "Cage 环境已就绪。")
				return cliExchange(ctx, c, hostCLIRequest{Action: "account-status"}, in, out, errOut, out)
			} else if status.Running {
				fmt.Fprintln(out, "Cage 环境正在准备。")
			} else {
				fmt.Fprintln(out, "Cage 环境已停止。")
			}
			return 0, nil
		}
		if status.Running || status.Ready || status.Job.Running || action == "stop" {
			if err = runCLILifecycle(ctx, c, "stop"); err != nil {
				return 1, err
			}
		}
		if action == "stop" {
			fmt.Fprintln(out, "Cage 环境已停止。")
			return 0, nil
		}
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
	status, err = cliWaitAccess(ctx, c, config, canOpen, errOut)
	if err != nil {
		return 1, err
	}
	if status.Job.Kind == "start" && status.Job.Running {
		if err := waitCLIJob(ctx, c, status.Job); err != nil {
			return 1, err
		}
		if err := cliCall(ctx, c, "GET", "/api/status", nil, &status); err != nil {
			return 1, err
		}
	}
	if !status.Ready {
		fmt.Fprintln(errOut, "正在启动 Cage 环境…")
		if err = runCLILifecycle(ctx, c, "start"); err != nil {
			return 1, err
		}
		if err = cliCall(ctx, c, "GET", "/api/status", nil, &status); err != nil {
			return 1, err
		}
		if !status.Ready {
			return 1, errors.New("环境尚未就绪，请在 Cage 工作台查看启动详情")
		}
	}
	if action == "restart" {
		fmt.Fprintln(out, "Cage 环境已就绪。")
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
	if action == "run" && !cliGlobal(args) {
		dir, err = os.Getwd()
		if err != nil {
			return 1, err
		}
	}
	request := hostCLIRequest{Action: action, Account: selector, Args: args, Directory: dir, TTY: tty}
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
	if errors.Is(err, errHostCLILogin) && tty {
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
