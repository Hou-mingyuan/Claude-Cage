package workbench

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sylvinus/agent-vm/internal/desktop"
	"github.com/sylvinus/agent-vm/internal/hostbridge"
	"github.com/sylvinus/agent-vm/internal/remotehost"
)

type remotePluginSessionServices struct {
	session        *remotehost.Session
	hostName       string
	approvalCancel context.CancelFunc
	approvalDone   chan struct{}
	syncer         *clientPluginSyncRunner
	bridge         *clientPluginBridgeManager
}

func newRemotePluginSessionServices(session *remotehost.Session, hostName, profileDir string, ask func(context.Context, string, string) (desktop.Scope, bool)) *remotePluginSessionServices {
	svc := &remotePluginSessionServices{
		session:  session,
		hostName: hostName,
		syncer:   newClientPluginSyncRunner(session, filepath.Join(profileDir, "plugins")),
		bridge:   newClientPluginBridgeManager(session, hostName, profileDir, ask),
	}
	if session.Manifest.Capabilities["remote_approvals"] {
		ctx, cancel := context.WithCancel(session.Context)
		done := make(chan struct{})
		svc.approvalCancel = cancel
		svc.approvalDone = done
		go func() {
			defer close(done)
			runRemoteApprovalClient(ctx, session, hostName, ask)
		}()
	}
	return svc
}

func (svc *remotePluginSessionServices) close() {
	if svc == nil {
		return
	}
	if svc.approvalCancel != nil {
		svc.approvalCancel()
	}
	if svc.syncer != nil {
		svc.syncer.close()
	}
	if svc.bridge != nil {
		svc.bridge.close()
	}
	if svc.approvalDone != nil {
		<-svc.approvalDone
	}
}

func (g *remoteGateway) ensureRemotePluginServices(session *remotehost.Session, hostName, profileDir string) *remotePluginSessionServices {
	if session == nil {
		return nil
	}
	g.mu.Lock()
	if g.pluginServices != nil && g.pluginServices.session == session && session.Context.Err() == nil {
		svc := g.pluginServices
		g.mu.Unlock()
		return svc
	}
	old := g.pluginServices
	ask := g.approvalAsk
	svc := newRemotePluginSessionServices(session, hostName, profileDir, ask)
	g.pluginServices = svc
	g.mu.Unlock()
	if old != nil {
		go old.close()
	}
	return svc
}

func (g *remoteGateway) closeRemotePluginServices() {
	g.mu.Lock()
	old := g.pluginServices
	g.pluginServices = nil
	g.mu.Unlock()
	if old != nil {
		old.close()
	}
}

func (g *remoteGateway) drainClosedRemotePluginServices() {
	g.mu.Lock()
	var old *remotePluginSessionServices
	if g.pluginServices != nil && g.pluginServices.session.Context.Err() != nil {
		old = g.pluginServices
		g.pluginServices = nil
	}
	g.mu.Unlock()
	if old != nil {
		old.close()
	}
}

func (s *Server) activeRemotePluginSession(connectionID string) (*remotehost.Session, string, error) {
	g := s.remote
	if g == nil {
		return nil, "", errors.New("远程连接服务不可用")
	}
	session, err := g.clientShareSession(connectionID)
	if err != nil {
		return nil, "", err
	}
	ctx := g.context()
	return session, ctx.Name, nil
}

// clientPluginsRequest handles local client endpoints for remote plugin sync
// status/triggers and reverse-bridged local host plugins.
func (s *Server) clientPluginsRequest(w http.ResponseWriter, r *http.Request, path string) {
	session, hostName, err := s.activeRemotePluginSession(requestConnection(r))
	if err != nil {
		http.Error(w, "工作环境已切换，请重新选择远程主机", http.StatusConflict)
		return
	}
	svc := s.remote.ensureRemotePluginServices(session, hostName, s.hostViewProfile())
	switch {
	case path == "plugins/sync" && r.Method == http.MethodGet:
		jsonReply(w, svc.syncer.snapshot())
	case path == "plugins/sync" && r.Method == http.MethodPost:
		if !s.hasTrustedView(r) {
			http.Error(w, "请在本机 Cage 窗口同步插件", http.StatusForbidden)
			return
		}
		svc.syncer.requestSync()
		jsonReply(w, svc.syncer.snapshot())
	case path == "plugins/bridge" && r.Method == http.MethodGet:
		jsonReply(w, svc.bridge.snapshot())
	case path == "plugins/bridge" && r.Method == http.MethodPost:
		if !s.hasTrustedView(r) {
			http.Error(w, "请在本机 Cage 窗口共享本机插件", http.StatusForbidden)
			return
		}
		var in struct {
			Plugin string `json:"plugin"`
			Shared bool   `json:"shared"`
		}
		if !readJSON(w, r, &in, 8<<10) {
			return
		}
		status, err := svc.bridge.setShared(r.Context(), in.Plugin, in.Shared)
		if err != nil {
			requestError(w, err)
			return
		}
		jsonReply(w, status)
	default:
		http.NotFound(w, r)
	}
}

// routeRemotePluginView proxies /plugin-views/<plugin>/<key>/<view>/<rest> from
// the currently active remote host and enforces strict client-side CSP and
// iframe sandboxing so remote headers can never weaken the frame boundary.
func (s *Server) routeRemotePluginView(w http.ResponseWriter, r *http.Request) bool {
	if s.remote == nil {
		return false
	}
	var session *remotehost.Session
	var remote bool
	if desktop := s.remote.desktop; desktop != nil {
		ctx := desktop.context()
		if ctx.Mode != "remote" {
			return false
		}
		var err error
		session, remote, err = desktop.acquire(ctx.ConnectionID)
		if err != nil || !remote || session == nil {
			http.NotFound(w, r)
			return true
		}
	} else {
		if !s.remote.isRemote() {
			return false
		}
		ctx := s.remote.context()
		var err error
		session, err = s.remote.manager.Acquire(ctx.ConnectionID)
		if err != nil || session == nil {
			http.NotFound(w, r)
			return true
		}
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return true
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, hostViewPrefix), "/", 4)
	if len(parts) != 4 {
		http.NotFound(w, r)
		return true
	}
	plugin, key, viewID, rest := parts[0], parts[1], parts[2], parts[3]
	if !hostbridge.ValidPluginName(plugin) || len(key) != 32 || viewID == "" || strings.ContainsAny(viewID, "/\\") {
		http.NotFound(w, r)
		return true
	}
	if rest != "" && (rest != path.Clean(rest) || rest == ".." || strings.HasPrefix(rest, "../") || path.IsAbs(rest) || strings.Contains(rest, "\\")) {
		http.NotFound(w, r)
		return true
	}

	reqCtx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	stop := context.AfterFunc(session.Context, cancel)
	defer stop()

	upstreamReq, err := http.NewRequestWithContext(reqCtx, r.Method, session.Origin+r.URL.Path, nil)
	if err != nil {
		http.NotFound(w, r)
		return true
	}
	res, err := session.Transport.RoundTrip(upstreamReq)
	if err != nil {
		http.Error(w, "无法读取远程插件页面", http.StatusBadGateway)
		return true
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		http.NotFound(w, r)
		return true
	}
	var data []byte
	if r.Method == http.MethodGet {
		data, err = io.ReadAll(io.LimitReader(res.Body, hostViewFileLimit+1))
		if err != nil || len(data) > hostViewFileLimit {
			http.Error(w, "远程插件页面资源无效或过大", http.StatusBadGateway)
			return true
		}
	}

	kind := "text/html; charset=utf-8"
	if rest != "" {
		kind = hostViewTypes[strings.ToLower(path.Ext(rest))]
		if kind == "" {
			kind = "application/octet-stream"
		}
	}
	own := s.origin + hostViewPrefix + plugin + "/"
	header := w.Header()
	header.Del("X-Frame-Options")
	header.Set("Content-Security-Policy", "default-src 'none'; script-src "+own+"; style-src "+own+" 'unsafe-inline'; img-src "+own+
		" data:; font-src "+own+"; connect-src 'none'; frame-src 'none'; worker-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors "+s.origin+
		"; sandbox allow-scripts")
	header.Set("Access-Control-Allow-Origin", "*")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Cache-Control", "no-store")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Content-Type", kind)
	if r.Method == http.MethodGet {
		header.Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
	}
	return true
}
