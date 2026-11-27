package workbench

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/hostbridge"
	"github.com/sylvinus/agent-vm/internal/usererror"
)

//go:embed linux-app-desktop.sh
var linuxAppDesktop string

//go:embed linux-app-install.sh
var linuxAppInstall string

//go:embed linux-app-remove.sh
var linuxAppRemove string

// desktopAppResolverLimit is the largest resolver program a plugin may have.
const desktopAppResolverLimit = 256 << 10

// desktopAppInstance is the environment's name for one plugin application.
// It has the form of a Desktop instance, so the display, clipboard, file
// chooser and downloads that serve Desktop instances serve it too, and it
// carries neither the plugin's name nor the product's.
func desktopAppInstance(plugin, app string) string {
	sum := sha256.Sum256([]byte("desktop-app\x00" + plugin + "\x00" + app))
	return hex.EncodeToString(sum[:12])
}

type desktopAppTarget struct {
	plugin string
	dir    string
	app    hostbridge.DesktopApp
	id     string
}

// desktopAppTarget finds an application of an enabled plugin that this
// version of the application can run.
func (s *Server) desktopAppTarget(plugin, app string) (desktopAppTarget, bool) {
	manifest, ok := s.hostViewManifest(plugin)
	if !ok || manifest.Kind != hostbridge.KindGuest || manifest.Contributes == nil {
		return desktopAppTarget{}, false
	}
	for _, candidate := range manifest.Contributes.DesktopApps {
		if candidate.ID == app {
			return desktopAppTarget{
				plugin: plugin,
				dir:    filepath.Join(s.hostViewProfile(), "plugins", plugin),
				app:    candidate,
				id:     desktopAppInstance(plugin, app),
			}, true
		}
	}
	return desktopAppTarget{}, false
}

// desktopAppVariables are the installer's inputs, as shell assignments of
// values the manifest check has already limited.
func desktopAppVariables(t desktopAppTarget) (string, error) {
	command, err := json.Marshal(t.app.Command)
	if err != nil {
		return "", err
	}
	source := t.app.Source
	resolver, kind, args := "", "", "[]"
	if len(source.Resolve) > 0 {
		program, err := os.ReadFile(filepath.Join(t.dir, filepath.FromSlash(source.Resolve[0])))
		if err != nil {
			return "", fmt.Errorf("读取插件的版本解析程序：%w", err)
		}
		if len(program) > desktopAppResolverLimit {
			return "", errors.New("插件的版本解析程序过大")
		}
		encoded, err := json.Marshal(source.Resolve[1:])
		if err != nil {
			return "", err
		}
		resolver, kind, args = base64.StdEncoding.EncodeToString(program), "sh", string(encoded)
		if strings.HasSuffix(strings.ToLower(source.Resolve[0]), ".py") {
			kind = "py"
		}
	}
	values := [][2]string{
		{"app_id", t.id},
		{"app_source", source.Type},
		{"app_command", string(command)},
		{"app_schemes", strings.Join(t.app.Schemes, " ")},
		{"app_packages", strings.Join(t.app.Packages, " ")},
		{"app_dependencies", cage.DesktopAppDependencies},
		{"app_repository", source.Repository},
		{"app_suite", source.Suite},
		{"app_components", strings.Join(source.Components, " ")},
		{"app_key", source.Key},
		{"app_fingerprint", source.Fingerprint},
		{"app_package", source.Package},
		{"app_hosts", strings.Join(source.Hosts, " ")},
		{"app_resolver", resolver},
		{"app_resolver_kind", kind},
		{"app_resolver_args", args},
	}
	var b strings.Builder
	for _, value := range values {
		b.WriteString(value[0] + "=" + shellescape.Quote(value[1]) + "\n")
	}
	return b.String(), nil
}

func desktopAppInstallScript(t desktopAppTarget) (string, error) {
	variables, err := desktopAppVariables(t)
	if err != nil {
		return "", err
	}
	return cage.ToolInstallPrelude() + cage.DesktopAppSupportScript() + variables + linuxAppInstall, nil
}

func desktopAppRemoveScript(t desktopAppTarget) string {
	return "set -euo pipefail\napp_id=" + t.id + "\n" + linuxAppRemove
}

func desktopAppTargetKey(t desktopAppTarget) string {
	return t.plugin + "/" + t.app.ID
}

// desktopAppSessionAllowedDuringJob reports whether starting or stopping a
// plugin desktop application may run alongside the active global workbench job.
// Package installation, updates and removals still serialize globally, while
// opening or closing another already-installed app only touches its own display
// service.
func desktopAppSessionAllowedDuringJob(job Job, targetKey string) bool {
	if !job.Running {
		return true
	}
	switch job.Kind {
	case "desktop-app-install", "desktop-app-update", "desktop-app-remove":
		return job.Target != "" && job.Target != targetKey
	case "claude-desktop-install", "claude-desktop-update", "claude-desktop-start", "claude-desktop-stop",
		"desktop-instance-start", "desktop-instance-stop",
		"environment-tools", "tailscale-install",
		"safety", "network-recovery", "proxy", "proxy-update", "proxy-network", "project":
		return true
	default:
		return false
	}
}

type desktopAppOp struct {
	id      string
	action  string
	running bool
	message string
	err     string
	cancel  context.CancelFunc
	done    chan struct{}
}

type desktopAppOpReply struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Target  string `json:"target"`
	Running bool   `json:"running"`
	Message string `json:"message"`
	Local   bool   `json:"local"`
}

type desktopAppOpManager struct {
	mu     sync.Mutex
	ops    map[string]*desktopAppOp
	closed bool
}

func (m *desktopAppOpManager) snapshot(appID string) (busy bool, action, message, errText string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	op := m.ops[appID]
	if op == nil {
		return false, "", "", ""
	}
	if op.running {
		return true, op.action, op.message, ""
	}
	return false, "", "", op.err
}

func (m *desktopAppOpManager) clearError(appID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if op := m.ops[appID]; op != nil && !op.running {
		op.err = ""
	}
}

func (m *desktopAppOpManager) cancelAndWait(appID string) {
	m.mu.Lock()
	op := m.ops[appID]
	var done chan struct{}
	if op != nil {
		if op.running && op.cancel != nil {
			op.cancel()
		}
		if op.running {
			done = op.done
		} else {
			op.err = ""
		}
	}
	m.mu.Unlock()
	if done != nil {
		<-done
	}
	m.clearError(appID)
}

func (m *desktopAppOpManager) cancelAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, op := range m.ops {
		if op.running && op.cancel != nil {
			op.cancel()
		}
		if !op.running {
			op.err = ""
		}
	}
}

func (m *desktopAppOpManager) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for _, op := range m.ops {
		if op.running && op.cancel != nil {
			op.cancel()
		}
	}
}

func (m *desktopAppOpManager) start(s *Server, target desktopAppTarget, action string, run func(context.Context) error) (desktopAppOpReply, int, error) {
	targetKey := desktopAppTargetKey(target)
	job := s.jobs.snapshot()
	if !desktopAppSessionAllowedDuringJob(job, targetKey) {
		if job.Target == targetKey && strings.HasPrefix(job.Kind, "desktop-app-") {
			return desktopAppOpReply{}, http.StatusConflict, fmt.Errorf("%s 正在进行维护操作，请稍候", target.app.Title)
		}
		return desktopAppOpReply{}, http.StatusConflict, fmt.Errorf("%w：当前正在%s。请等待完成，或先停止当前操作。", errJobBusy, jobName(job.Kind))
	}
	activityCtx, finish, err := s.beginActivity(context.Background())
	if err != nil {
		return desktopAppOpReply{}, http.StatusServiceUnavailable, err
	}
	message := "正在关闭 " + target.app.Title + "…"
	if action == "start" {
		message = "正在打开 " + target.app.Title + "…"
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		finish()
		return desktopAppOpReply{}, http.StatusServiceUnavailable, errJobsClosed
	}
	if existing := m.ops[target.id]; existing != nil && existing.running {
		if existing.action == action {
			reply := desktopAppOpReply{
				ID:      existing.id,
				Kind:    "desktop-app-" + existing.action,
				Target:  targetKey,
				Running: true,
				Message: existing.message,
				Local:   true,
			}
			m.mu.Unlock()
			finish()
			return reply, http.StatusOK, nil
		}
		busyMessage := target.app.Title + " 正在打开，请稍候"
		if existing.action == "stop" {
			busyMessage = target.app.Title + " 正在关闭，请稍候"
		}
		m.mu.Unlock()
		finish()
		return desktopAppOpReply{}, http.StatusConflict, errors.New(busyMessage)
	}
	if m.ops == nil {
		m.ops = make(map[string]*desktopAppOp)
	}
	opCtx, cancel := context.WithCancel(activityCtx)
	op := &desktopAppOp{
		id:      randomToken()[:12],
		action:  action,
		running: true,
		message: message,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	m.ops[target.id] = op
	reply := desktopAppOpReply{
		ID:      op.id,
		Kind:    "desktop-app-" + action,
		Target:  targetKey,
		Running: true,
		Message: message,
		Local:   true,
	}
	m.mu.Unlock()

	go func() {
		defer func() {
			cancel()
			close(op.done)
			finish()
		}()
		runErr := run(opCtx)
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.ops[target.id] != op {
			return
		}
		op.running = false
		op.cancel = nil
		op.message = ""
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			op.err = usererror.Display(jobName("desktop-app-"+action), runErr, "仍无法处理时，"+failureHelp).Error()
			diagnostics.Log("error", "desktop-app", fmt.Sprintf("%s %s: %v", target.id, action, runErr))
		} else {
			op.err = ""
		}
	}()
	return reply, http.StatusOK, nil
}

type desktopAppState struct {
	ID        string `json:"id"`
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	Previous  string `json:"previous_version,omitempty"`
	Running   bool   `json:"running"`
	Failed    bool   `json:"failed"`
	// FailureReason is oom-kill when the system ended the application to free memory.
	FailureReason string `json:"failure_reason,omitempty"`
	// Busy, Action, Message and Error report an in-flight or failed per-app
	// start/stop operation independently of global maintenance jobs.
	Busy    bool   `json:"busy,omitempty"`
	Action  string `json:"action,omitempty"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}

const desktopAppSnapshot = `python3 -I - %s <<'APP_STATE'
import json, subprocess, sys
from pathlib import Path
instance = sys.argv[1]
def run(*args):
    return subprocess.run(args, capture_output=True, text=True).stdout.strip()
record = {}
try:
    record = json.loads(Path('/usr/local/lib/desktop-apps/' + instance + '/app.json').read_text())
except (OSError, ValueError):
    pass
unit = 'desktop-' + instance + '.service'
state = run('systemctl', 'is-active', unit)
result = run('systemctl', 'show', '--property=Result', '--value', unit) if state == 'failed' else ''
print(json.dumps({'id': instance, 'installed': bool(record.get('command')), 'version': record.get('version', ''),
    'previous_version': record.get('previous_version', ''), 'running': state == 'active', 'failed': state == 'failed',
    'failure_reason': 'oom-kill' if result == 'oom-kill' else ''}))
APP_STATE`

// desktopApp serves one plugin application: its state, the operations on it
// and, while it runs, its display, clipboard and input method.
func (s *Server) desktopApp(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	get := map[string]bool{"status": true, "vnc": true, "vnc-credentials": true, "clipboard": true, "input": true}
	post := map[string]bool{"install": true, "update": true, "remove": true, "start": true, "stop": true, "resize": true}
	if r.Method == http.MethodGet && !get[action] || r.Method == http.MethodPost && !post[action] {
		http.NotFound(w, r)
		return
	}
	target, ok := s.desktopAppTarget(r.PathValue("plugin"), r.PathValue("app"))
	if !ok {
		http.Error(w, "插件未启用，或这个版本的 Cage 还不能运行它的应用", http.StatusNotFound)
		return
	}
	var size displaySize
	var scale string
	if action == "resize" {
		if size, scale, ok = readDisplaySize(w, r); !ok {
			return
		}
	} else if r.Method == http.MethodPost {
		var input struct{}
		if !readJSON(w, r, &input) {
			return
		}
	}
	if !s.accountDesktopsSupported() {
		http.Error(w, "此环境暂不支持插件桌面应用", http.StatusNotImplemented)
		return
	}
	if !s.vmRunning() {
		http.Error(w, "请先启动 Cage 环境", 409)
		return
	}
	run := "/run/desktop-" + target.id
	switch action {
	case "status":
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		b, err := s.guestQuick(ctx, fmt.Sprintf(desktopAppSnapshot, target.id))
		var state desktopAppState
		if err != nil || json.Unmarshal(b, &state) != nil || state.ID != target.id {
			http.Error(w, "读取应用状态失败，请确认环境仍在运行", http.StatusBadGateway)
			return
		}
		if state.Running {
			s.desktopAppOps.clearError(target.id)
		}
		state.Busy, state.Action, state.Message, state.Error = s.desktopAppOps.snapshot(target.id)
		jsonReply(w, state)
		return
	case "vnc":
		s.streamDesktopSocket(w, r, run+"/vnc.sock")
		return
	case "vnc-credentials":
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		// A session with a settings manager renders at the density its viewer
		// names. One started by an earlier version keeps 96 DPI: its viewer
		// sizes the display in CSS pixels.
		b, err := s.guest(ctx, "systemctl is-active --quiet desktop-"+target.id+".service && cat "+run+`/password && { if test -S "$HOME/.local/share/desktop-apps/`+target.id+`/input/.cache/cage-input/input.sock"; then printf 1; else printf 0; fi; } && if test -f `+run+`/xsettings.pid; then printf ' %s' "$(cat `+run+`/base-display-scale 2>/dev/null)"; fi`)
		reply, ok := accountDesktopCredentials(b)
		if err != nil || !ok {
			http.Error(w, "应用尚未打开", 409)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		jsonReply(w, reply)
		return
	case "clipboard":
		s.sharedDesktopClipboard(w, r, target.id)
		return
	case "input":
		s.streamDesktopCommand(w, r, `exec env HOME="$HOME/.local/share/desktop-apps/`+target.id+`/input" /usr/local/bin/cage-native-input --relay`)
		return
	case "resize":
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		// The application's unit has the runtime directory and display record
		// of a Desktop instance. It shares the environment's namespaces, so
		// entering those of its main process changes nothing.
		output, err := s.guestQuick(ctx, accountDesktopResizeScript(target.id, size.Width, size.Height, scale))
		if err != nil {
			http.Error(w, "应用画面尺寸未能调整", 502)
			return
		}
		displayResizeReply(w, size, scale, output)
		return
	case "start", "stop":
		reply, code, err := s.desktopAppOps.start(s, target, action, func(ctx context.Context) error {
			return s.runDesktopAppSession(ctx, target, action)
		})
		if err != nil {
			http.Error(w, err.Error(), code)
			return
		}
		jsonReply(w, reply)
		return
	}
	s.launchRequestWithTarget(w, r, "desktop-app-"+action, desktopAppTargetKey(target), func() error {
		return s.changeDesktopApp(target, action)
	})
}

func (s *Server) changeDesktopApp(t desktopAppTarget, action string) error {
	s.desktopAppOps.cancelAndWait(t.id)
	title := t.app.Title
	switch action {
	case "install", "update":
		s.mu.Lock()
		allowed := s.toolsNetworkAllowedLocked()
		tls12Compat := s.preferences.InstallTLS12Compat
		s.mu.Unlock()
		if !allowed {
			return errors.New("工作台正在退出，请稍后重试安装")
		}
		stopMonitor := s.keepNetworkAliveDuringInstall(s.operationContext())
		defer stopMonitor()
		plan, err := prepareDesktopAppInstall(t, reviewedDesktopApps[t.plugin])
		if err != nil {
			return err
		}
		if plan.unreviewedPlugin != "" {
			_, _ = fmt.Fprintf(s.jobs.execution(), "提示：插件 %s 的当前版本或内容未经此版 Cage 审核，额外官方下载源未开放；若下载被拦截，请在插件市场更新插件或升级 Cage。\n", plan.unreviewedPlugin)
		}
		script := plan.script
		if action == "update" {
			s.setMessage("正在把 " + title + " 更新到官方最新版本，可在操作详情查看进度…")
			script = "set -e\nsudo systemctl stop desktop-" + t.id + ".service 2>/dev/null || :\n" + cage.InstallTLS12Prelude(tls12Compat) + script
		} else {
			s.setMessage("正在下载并安装 " + title + "，可在操作详情查看进度…")
			script = cage.InstallTLS12Prelude(tls12Compat) + script
		}
		opts := maintenanceOptions{
			installHTTPSHosts: plan.httpsHosts,
			allowedAPTAppID:   plan.allowedAPTAppID,
			unreviewedPlugin:  plan.unreviewedPlugin,
		}
		return runInstallMaintenanceScript(s.operationContext(), s.configPath, script, opts, s.jobs.execution(), s.jobs.execution())
	case "remove":
		s.setMessage("正在卸载 " + title + "…")
		return s.runDesktopScript(desktopAppRemoveScript(t), 5*time.Minute)
	default:
		return s.runDesktopAppSession(s.operationContext(), t, action)
	}
}

func (s *Server) runDesktopAppSession(ctx context.Context, t desktopAppTarget, action string) error {
	if s.desktopAppOpRunner != nil {
		return s.desktopAppOpRunner(ctx, t, action)
	}
	if action == "start" {
		if err := s.runDesktopAppSupport(ctx, t); err != nil {
			return err
		}
		key, err := s.account.desktopKey()
		if err != nil {
			return err
		}
		defer clear(key)
		return s.runDesktopApp(ctx, t, "start", key)
	}
	return s.runDesktopApp(ctx, t, "stop", nil)
}

func (s *Server) runDesktopAppSupport(ctx context.Context, t desktopAppTarget) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	unit := "desktop-app-support-" + t.id
	command := fmt.Sprintf("sudo systemd-run --quiet --pipe --wait --collect --unit=%s --property=User=cage --property=RuntimeMaxSec=30 --property=KillMode=control-group --setenv=HOME=/home/cage --working-directory=/home/cage /bin/bash -se", unit)
	var err error
	command, err = s.linuxTemplate(command)
	if err != nil {
		return err
	}
	script, err := s.linuxTemplate(cage.DesktopAppSupportScript())
	if err != nil {
		return err
	}
	cmd := s.sshCommand(ctx, "-T", s.sshTarget(), "--", command)
	cmd.Stdin = strings.NewReader(script)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			cleanup, finish := context.WithTimeout(context.Background(), 5*time.Second)
			defer finish()
			_, _ = s.guest(cleanup, "sudo systemctl stop "+unit+".service")
			return ctx.Err()
		}
		return executionFailure("准备 "+t.app.Title, err, output.String())
	}
	return nil
}

func (s *Server) runDesktopApp(ctx context.Context, t desktopAppTarget, action string, key []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 100*time.Second)
	defer cancel()
	command := fmt.Sprintf("exec /bin/bash -c %s -- %s %s", shellescape.Quote(linuxAppDesktop), t.id, action)
	cmd := s.sshCommand(ctx, "-T", s.sshTarget(), "--", command)
	cmd.Stdin = bytes.NewReader(key)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if bytes.Contains(output.Bytes(), []byte("应用尚未安装")) {
			return errors.New("请先安装 " + t.app.Title)
		}
		if bytes.Contains(output.Bytes(), []byte("应用程序缺失")) {
			return errors.New(t.app.Title + " 的程序缺失，请点击“更新 / 修复安装”")
		}
		return executionFailure("操作 "+t.app.Title, errors.Join(ctx.Err(), err), output.String())
	}
	return nil
}
