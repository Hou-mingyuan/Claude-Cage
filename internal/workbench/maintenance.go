package workbench

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"al.essio.dev/pkg/shellescape"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/containerenv"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/usererror"
)

// Only distribution/tool origins are reachable through this temporary channel.
// Guest business traffic continues through EgressGate without any grant here.
var maintenanceOrigins = map[string]bool{
	"deb.debian.org": true, "security.debian.org": true,
	"github.com": true, "objects.githubusercontent.com": true, "release-assets.githubusercontent.com": true,
	"registry.npmjs.org": true, "downloads.claude.ai": true, "deb.nodesource.com": true,
}

type maintenanceProxy struct {
	token             string
	installHTTPSHosts []string
	ctx               context.Context
	dial              func(context.Context, string, string) (net.Conn, error)
	transport         *http.Transport
	onReject          func(destination string)
	mu                sync.Mutex
	connections       map[net.Conn]bool
	blocked           []string
	closed            bool
}

type maintenanceOptions struct {
	installHTTPSHosts []string
	allowedAPTAppID   string
	unreviewedPlugin  string
}

type syncWriter struct {
	mu  sync.Mutex
	dst io.Writer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dst.Write(p)
}

func (p *maintenanceProxy) recordBlocked(destination string) {
	destination = strings.TrimSpace(destination)
	if destination == "" {
		return
	}
	p.mu.Lock()
	first := !slices.Contains(p.blocked, destination)
	if first {
		p.blocked = append(p.blocked, destination)
	}
	onReject := p.onReject
	p.mu.Unlock()
	if first && onReject != nil {
		onReject(destination)
	}
}

func (p *maintenanceProxy) blockedDestinations() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.blocked)
}

func (p *maintenanceProxy) allowsDestination(address string, connect bool) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if connect && port != "443" {
		return false
	}
	if !connect && port != "80" {
		return false
	}
	host = strings.ToLower(host)
	switch host {
	case "mirrors.nju.edu.cn", "mirrors.ustc.edu.cn", "cdn.npmmirror.com", "api.snapcraft.io", "storage.snapcraftcontent.com", "canonical-lgw01.cdn.snapcraftcontent.com", "canonical-lcy01.cdn.snapcraftcontent.com", "canonical-lcy02.cdn.snapcraftcontent.com", "canonical-bos01.cdn.snapcraftcontent.com", "cloudfront.cdn.snapcraftcontent.com", "codeload.github.com":
		return connect
	}
	return maintenanceOrigins[host] || (connect && slices.Contains(p.installHTTPSHosts, host))
}
func (p *maintenanceProxy) track(conn net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		conn.Close()
		return false
	}
	p.connections[conn] = true
	return true
}
func (p *maintenanceProxy) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for conn := range p.connections {
		conn.Close()
	}
	p.transport.CloseIdleConnections()
}
func (p *maintenanceProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	expected := "Basic " + base64.StdEncoding.EncodeToString([]byte("cage-install:"+p.token))
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Proxy-Authorization")), []byte(expected)) != 1 {
		w.Header().Set("Proxy-Authenticate", `Basic realm="Cage maintenance"`)
		http.Error(w, "Maintenance authentication required", 407)
		return
	}
	if r.Method == http.MethodConnect {
		if !p.allowsDestination(r.Host, true) {
			p.recordBlocked(r.Host)
			http.Error(w, "Not a Cage software source", 403)
			return
		}
		remote, err := p.dial(p.ctx, "tcp", r.Host)
		if err != nil {
			http.Error(w, "Software source proxy unavailable", 502)
			return
		}
		if !p.track(remote) {
			return
		}
		defer func() { remote.Close(); p.mu.Lock(); delete(p.connections, remote); p.mu.Unlock() }()
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "Tunnel unavailable", 500)
			return
		}
		local, buffer, err := hijacker.Hijack()
		if err != nil {
			return
		}
		if !p.track(local) {
			return
		}
		defer func() { local.Close(); p.mu.Lock(); delete(p.connections, local); p.mu.Unlock() }()
		if _, err = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		if err = buffer.Flush(); err != nil {
			return
		}
		done := make(chan struct{})
		go func() { _, _ = io.Copy(remote, buffer); remote.Close(); close(done) }()
		_, _ = io.Copy(local, remote)
		local.Close()
		remote.Close()
		<-done
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Read-only software source", 405)
		return
	}
	address := r.URL.Host
	if r.URL.Port() == "" {
		address = net.JoinHostPort(r.URL.Hostname(), "80")
	}
	if r.URL.Scheme != "http" || r.URL.User != nil || !p.allowsDestination(address, false) {
		blocked := address
		if blocked == "" {
			blocked = r.URL.Host
		}
		p.recordBlocked(blocked)
		http.Error(w, "Not a Cage software source", 403)
		return
	}
	request := r.Clone(p.ctx)
	request.RequestURI = ""
	request.Header = r.Header.Clone()
	request.Header.Del("Proxy-Authorization")
	request.Header.Del("Proxy-Connection")
	response, err := p.transport.RoundTrip(request)
	if err != nil {
		http.Error(w, "Software source proxy unavailable", 502)
		return
	}
	defer response.Body.Close()
	for name, values := range response.Header {
		if strings.EqualFold(name, "Connection") {
			continue
		}
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

const maintenancePort = 18961

// The script and one-use local credential travel on SSH stdin, never argv.
// APT_CONFIG is passed explicitly through sudo by the owned install scripts.
func maintenanceScript(script, token string, container bool) string {
	return maintenanceScriptWithAPTScope(script, token, container, "")
}

// maintenanceScriptWithAPTScope scopes APT's sources.list.d during maintenance
// so vendor repositories registered by other desktop-app plugins (whose origins
// are not opened for this operation) do not cause 403 warnings or update
// failures. The helper stays shell-local and is never exported to user apps.
func maintenanceScriptWithAPTScope(script, token string, container bool, allowedAPTAppID string) string {
	if container {
		// Debian container images clean the default archive cache on apt update
		// and after dpkg. Keep managed downloads outside those cleanup hooks.
		script = `sudo install -d -m 0755 /var/cache/apt/install-archives
printf '%s\n' 'Dir::Cache::archives "/var/cache/apt/install-archives";' >> "$CAGE_APT_CONFIG"
` + script
	}
	if !validClaudeID(allowedAPTAppID) {
		allowedAPTAppID = ""
	}
	return fmt.Sprintf(`set -euo pipefail
umask 077
cage_maintenance_dir=$(mktemp -d)
trap 'rm -rf "$cage_maintenance_dir"' EXIT
export CAGE_INSTALL_PROXY='http://cage-install:%s@127.0.0.1:%d'
export CAGE_APT_CONFIG="$cage_maintenance_dir/apt.conf"
printf 'Acquire::http::Proxy "%%s";\nAcquire::https::Proxy "%%s";\nAcquire::Retries "1";\nAcquire::http::Timeout "15";\nAcquire::https::Timeout "15";\n' "$CAGE_INSTALL_PROXY" "$CAGE_INSTALL_PROXY" > "$CAGE_APT_CONFIG"
cage_apt_allowed_app_id=%s
cage_refresh_apt_sources() {
  local src_dir="${cage_apt_sources_dir:-/etc/apt/sources.list.d}"
  local apps_dir="${cage_desktop_apps_dir:-/usr/local/lib/desktop-apps}"
  local allowed="${cage_apt_allowed_app_id:-}"
  local filtered="$cage_maintenance_dir/sources.list.d"
  [ -d "$src_dir" ] || return 0
  local exclude=""
  local entry base id vendor_excluded
  for entry in "$src_dir"/desktop-app-*.list; do
    [ -e "$entry" ] || [ -L "$entry" ] || continue
    base=$(basename "$entry")
    id=${base#desktop-app-}
    id=${id%%.list}
    if [ -n "$allowed" ] && [ "$id" = "$allowed" ]; then
      continue
    fi
    exclude="$exclude$base"$'\n'
  done
  if [ -d "$apps_dir" ] && command -v python3 >/dev/null 2>&1; then
    vendor_excluded=$(python3 -I - "$apps_dir" "$src_dir" "$allowed" <<'CAGE_APT_FILTER' 2>/dev/null || :
import json, sys
from pathlib import Path
apps_dir, src_dir, allowed = Path(sys.argv[1]), sys.argv[2].rstrip("/"), sys.argv[3]
prefix = src_dir + "/"
for record in sorted(apps_dir.glob("*/app.json")):
    if allowed and record.parent.name == allowed:
        continue
    try:
        data = json.loads(record.read_text())
    except (OSError, ValueError):
        continue
    for path in data.get("vendor_files", []):
        if isinstance(path, str) and path.startswith(prefix):
            name = path[len(prefix):]
            if name and "/" not in name:
                print(name)
CAGE_APT_FILTER
)
    if [ -n "$vendor_excluded" ]; then
      exclude="$exclude$vendor_excluded"$'\n'
    fi
  fi
  if [ -z "$exclude" ]; then
    rm -rf "$filtered"
    if [ -f "$CAGE_APT_CONFIG" ]; then
      (
        umask 077
        grep -v '^Dir::Etc::sourceparts ' "$CAGE_APT_CONFIG" > "$CAGE_APT_CONFIG.new" || :
        mv -f "$CAGE_APT_CONFIG.new" "$CAGE_APT_CONFIG"
      )
    fi
    return 0
  fi
  rm -rf "$filtered"
  mkdir -m 0755 "$filtered"
  for entry in "$src_dir"/*; do
    [ -f "$entry" ] || continue
    base=$(basename "$entry")
    if grep -qxF -- "$base" <<<"$exclude"; then
      continue
    fi
    ln -s "$entry" "$filtered/$base"
  done
  (
    umask 077
    grep -v '^Dir::Etc::sourceparts ' "$CAGE_APT_CONFIG" > "$CAGE_APT_CONFIG.new" || :
    printf 'Dir::Etc::sourceparts "%%s";\n' "$filtered" >> "$CAGE_APT_CONFIG.new"
    mv -f "$CAGE_APT_CONFIG.new" "$CAGE_APT_CONFIG"
  )
}
cage_refresh_apt_sources
unset ALL_PROXY all_proxy NO_PROXY no_proxy FTP_PROXY ftp_proxy
export http_proxy="$CAGE_INSTALL_PROXY" https_proxy="$CAGE_INSTALL_PROXY" HTTP_PROXY="$CAGE_INSTALL_PROXY" HTTPS_PROXY="$CAGE_INSTALL_PROXY"
(
umask 022
%s
)
`, token, maintenancePort, shellescape.Quote(allowedAPTAppID), script)
}

func maintenanceProxyFailure(action string, cause error, blocked []string, unreviewedPlugin, log string) error {
	if len(blocked) == 0 {
		return nil
	}
	lower := strings.ToLower(log)
	if !strings.Contains(lower, "403") && !strings.Contains(lower, "forbidden") {
		return nil
	}
	hosts := strings.Join(blocked, "、")
	if unreviewedPlugin != "" {
		reason := "该插件版本未经当前版本 Cage 审核，额外下载源未开放"
		detail := fmt.Sprintf("安装代理已拒绝未放行的下载源（%s）。插件 %s 的当前版本或文件与本版 Cage 已审核内容不一致。请在插件市场更新或重新安装该插件，或升级 Cage 后重试。", hosts, unreviewedPlugin)
		return usererror.New(action, reason, detail, cause)
	}
	reason := "安装代理已拦截未放行的下载源"
	detail := fmt.Sprintf("安装代理已拒绝访问 %s（这是 Cage 的安装代理策略限制，而非上游网络故障）。请升级 Cage 或确认插件的官方下载源是否已变更。", hosts)
	return usererror.New(action, reason, detail, cause)
}

// Read the workbench preference at service startup so CLI and desktop launches
// use the same setting without modifying the network configuration.
func StartGuestServices(ctx context.Context, configPath string, stdout, stderr io.Writer) error {
	prefs := Preferences{BalancedDisplay: true}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(configPath), "workbench.json"))
	if err == nil {
		prefs.BalancedDisplay = false
		if err = json.Unmarshal(data, &prefs); err != nil {
			return fmt.Errorf("读取工作台画质设置失败: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	scale := "2"
	if prefs.BalancedDisplay {
		scale = "1.5"
	}
	themePrefix := ""
	if v := resolvedGuestTheme(filepath.Dir(configPath)); v != "" {
		themePrefix = "export " + cage.GuestThemeEnv + "=" + v + "\n"
	}
	cfg, err := cage.Load(configPath)
	if err != nil {
		return err
	}
	return runMaintenanceScript(ctx, configPath, cfg, "export CAGE_DISPLAY_SCALE="+scale+"\n"+themePrefix+cfg.StartScript(), maintenanceOptions{}, stdout, stderr)
}

func RunMaintenanceScript(ctx context.Context, configPath, script string, stdout, stderr io.Writer) error {
	return runInstallMaintenanceScript(ctx, configPath, script, maintenanceOptions{}, stdout, stderr)
}

func runInstallMaintenanceScript(ctx context.Context, configPath, script string, opts maintenanceOptions, stdout, stderr io.Writer) error {
	cfg, err := cage.Load(configPath)
	if err != nil {
		return err
	}
	return runMaintenanceScript(ctx, configPath, cfg, cfg.GuestProfile.Linux(script), opts, stdout, stderr)
}

func runMaintenanceScript(ctx context.Context, configPath string, cfg cage.Config, script string, opts maintenanceOptions, stdout, stderr io.Writer) error {
	profile := filepath.Dir(configPath)
	if err := cfg.RequireManagedProxy(profile); err != nil {
		return err
	}
	egress, err := cage.NewEgress(cfg)
	if err != nil {
		return err
	}
	// Each download detects inactivity; cancellation still closes forwarding
	// and stops the guest operation without imposing a total install duration.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	var safeStderr io.Writer
	if stderr != nil {
		safeStderr = &syncWriter{dst: stderr}
	}
	proxy := &maintenanceProxy{
		token:             randomToken(),
		installHTTPSHosts: opts.installHTTPSHosts,
		ctx:               ctx,
		dial:              egress.DialContext,
		connections:       make(map[net.Conn]bool),
		onReject: func(destination string) {
			diagnostics.Log("warn", "maintenance", fmt.Sprintf("blocked software source=%s", destination))
			if safeStderr != nil {
				_, _ = fmt.Fprintf(safeStderr, "安装代理已拒绝未放行的下载源：%s\n", destination)
			}
		},
	}
	proxy.transport = &http.Transport{Proxy: nil, DialContext: egress.DialContext, DisableKeepAlives: true}
	server := &http.Server{Handler: proxy, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	defer proxy.close()
	stop := context.AfterFunc(ctx, func() { _ = server.Close(); proxy.close() })
	defer stop()
	s := &Server{profile: profile}
	forward := fmt.Sprintf("127.0.0.1:%d:%s", maintenancePort, listener.Addr().String())
	command := "sudo systemd-run --quiet --pipe --wait --collect --unit=cage-maintenance-operation --property=User=cage --property=RuntimeMaxSec=infinity --property=KillMode=control-group --setenv=HOME=/home/cage "
	command = cfg.GuestProfile.Linux(command) + "--working-directory=" + shellescape.Quote(guestPath(s.environmentDir())) + " /bin/bash -se"
	cmd := s.sshCommandWithForwarding(ctx, "-o", "ExitOnForwardFailure=yes", "-R", forward, "-T", s.sshTarget(), "--", command)
	cmd.Stdin = strings.NewReader(maintenanceScriptWithAPTScope(cage.DebianPrepareScript+"\n"+script, proxy.token, containerenv.Configured(profile), opts.allowedAPTAppID))
	var output failureOutput
	cmd.Stdout = output.writer(stdout)
	cmd.Stderr = output.writer(safeStderr)
	if err = cmd.Run(); err != nil {
		if ctx.Err() != nil {
			cleanup, finish := context.WithTimeout(context.Background(), 5*time.Second)
			defer finish()
			_, _ = s.guest(cleanup, "sudo systemctl stop cage-maintenance-operation.service")
			return ctx.Err()
		}
		diagnostics.Log("error", "maintenance", fmt.Sprintf("software installation error=%v", err))
		if failure := maintenanceProxyFailure("安装或更新环境软件", err, proxy.blockedDestinations(), opts.unreviewedPlugin, output.String()); failure != nil {
			return failure
		}
		if failure := packageInstallFailure("安装或更新环境软件", err, output.String()); failure != nil {
			return failure
		}
		return executionFailure("安装或更新环境软件", err, output.String())
	}
	return nil
}
