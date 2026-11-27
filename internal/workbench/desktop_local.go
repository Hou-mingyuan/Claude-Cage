package workbench

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/sylvinus/agent-vm/internal/containerenv"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
	"github.com/sylvinus/agent-vm/internal/remotehost"
)

type desktopLocal struct {
	config, profile, executable string
	adoptIdleStandalone         bool
	mu                          sync.Mutex
	ownedLaunch                 string
}

func NewDesktopLocalController(configPath, fullExecutable string) DesktopLocalController {
	return &desktopLocal{
		config:              configPath,
		profile:             filepath.Dir(configPath),
		executable:          fullExecutable,
		adoptIdleStandalone: os.Getenv("CAGE_DESKTOP_OWNER_PID") != "",
	}
}

func (d *desktopLocal) available() bool {
	info, err := os.Stat(d.executable)
	return filepath.IsAbs(d.executable) && err == nil && info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode()&0111 != 0)
}

const desktopLocalOperationBusy = "本机正在执行操作，请稍后重试"

func (c *controllerClient) localControl(ctx context.Context, action string, input desktopLocalRequest) (desktopLocalReply, error) {
	var reply desktopLocalReply
	input.Token = c.record.Token
	path := "/api/desktop-local/" + action
	deadline := time.Now().Add(3 * time.Second)
	status, reason, err := c.callReason(ctx, http.MethodPost, path, input, &reply)
	// A routine monitor briefly holds the operation lock. The identical
	// release request still checks controller identity, job and VM state.
	for action == "release" && err == nil && status == http.StatusConflict && reason == desktopLocalOperationBusy && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return reply, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		status, reason, err = c.callReason(ctx, http.MethodPost, path, input, &reply)
	}
	if err != nil {
		return reply, err
	}
	switch status {
	case http.StatusOK:
		if action == "status" && (reply.OwnerID != localControllerID(c.record.Token) || reply.Identity.PID != c.record.PID || reply.Identity.Protocol != controlProtocol || !reply.Identity.matches(c.record.Controller) || !remoteInstanceID.MatchString(reply.InstanceID)) {
			return reply, errors.New("无法确认本机控制服务身份")
		}
		return reply, nil
	case http.StatusLocked:
		return reply, errDesktopLocalLocked
	case http.StatusNotFound:
		return reply, errors.New("当前本机后台版本不支持环境切换，请先在原工作台保存任务并完成后台更新")
	case http.StatusConflict:
		return reply, errors.New("本机仍有操作或状态已变化，请重新确认")
	default:
		return reply, errors.New("本机控制服务未接受此操作")
	}
}

// offlineStatus uses only existing private records. A live lock, orphan VM,
// or proxy without a reachable controller is unknown, never a released state.
func (d *desktopLocal) offlineStatus(ctx context.Context, available bool) DesktopLocalStatus {
	st := DesktopLocalStatus{Available: available, Message: "本机状态待确认，请返回本机检查已有后台"}
	free, err := controllerLockStatus(d.profile)
	if err != nil {
		st.Message = "无法检查本机控制锁或修复旧版权限，请检查配置目录的访问权限：" + err.Error()
		return st
	}
	if !free {
		st.ServiceRunning = true
		return st
	}
	// A container host requires an explicit engine probe. Listing the desktop
	// selector must not contact or start a container engine.
	if containerenv.Configured(d.profile) {
		return st
	}
	running, err := controllerVMState(d.profile)
	if err != nil || running {
		st.Running = running
		return st
	}
	if managedproxy.ConfirmStopped(ctx, d.profile) != nil {
		return st
	}
	st.Known, st.Message = true, ""
	return st
}

func (d *desktopLocal) Status(ctx context.Context) (DesktopLocalStatus, error) {
	available := d.available()
	free, lockErr := controllerLockStatus(d.profile)
	if free || lockErr != nil {
		return d.offlineStatus(ctx, available), nil
	}
	c, err := openController(d.profile)
	if errors.Is(err, errControllerUnavailable) {
		return d.offlineStatus(ctx, available), nil
	}
	if err != nil {
		return DesktopLocalStatus{Available: available, Message: "本机后台记录无效，请检查本机配置"}, nil
	}
	defer c.close()
	view, err := c.localControl(ctx, "status", desktopLocalRequest{})
	if err != nil {
		if !processExists(c.record.PID) {
			return d.offlineStatus(ctx, available), nil
		}
		return DesktopLocalStatus{Available: available, ServiceRunning: true, Message: "无法确认已有本机后台的状态或版本"}, nil
	}
	view.Available = available
	return view.DesktopLocalStatus, nil
}

// Attach opens a view of the observed controller. A window-only launch must
// never bootstrap or replace a workload if it vanishes during the handshake.
func (d *desktopLocal) Attach(ctx context.Context, controllerID string) (*remotehost.Session, error) {
	c, err := connectController(d.profile)
	if err != nil {
		return nil, err
	}
	defer c.close()
	want, err := currentController()
	if err != nil || !want.matches(c.launch.Controller) || !sameControllerConfig(c.launch.ConfigPath, d.config) || c.launch.ClientOnly || c.launch.DesktopGateway {
		return nil, errors.New("本机后台身份已变化，请重新选择")
	}
	bootstrap, cancel := context.WithTimeout(ctx, localBootstrapTimeout)
	defer cancel()
	view, err := c.localControl(bootstrap, "status", desktopLocalRequest{})
	if err != nil {
		return nil, err
	}
	if !view.Known || controllerID == "" || view.ControllerID != controllerID {
		return nil, errors.New("本机后台已变化，请重新选择")
	}
	return d.localSession(ctx, bootstrap, c, view)
}

func (d *desktopLocal) Connect(ctx context.Context) (_ *remotehost.Session, returned error) {
	st, err := d.Status(ctx)
	if err != nil {
		return nil, err
	}
	if !st.Available {
		return nil, errors.New("此安装缺少本机运行组件")
	}
	if !st.Known && st.ServiceRunning && st.ControllerID == "" {
		wait, cancel := context.WithTimeout(ctx, 12*time.Second)
		c, waitErr := waitControllerReady(wait, d.profile)
		cancel()
		if c != nil {
			c.close()
		}
		if waitErr != nil && !errors.Is(waitErr, errControllerUnavailable) {
			return nil, waitErr
		}
		st, err = d.Status(ctx)
		if err != nil {
			return nil, err
		}
	}
	if !st.Known && st.ServiceRunning && st.ControllerID == "" {
		handed, err := d.handoffOldLocal(ctx)
		if err != nil {
			return nil, err
		}
		if handed {
			st, err = d.Status(ctx)
			if err != nil || !st.Known || st.ServiceRunning {
				return nil, errors.New("已有本机服务尚未完全退出，请刷新状态后重试")
			}
		}
	}
	// Explicit local selection may reattach an orphan workload to its owner.
	// A live or inaccessible lock with no reachable service cannot be taken over.
	if !st.Known && !st.ServiceRunning && !controllerLockFree(d.profile) {
		return nil, errors.New(st.Message)
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if session := desktopSessionID(); session != "" && st.Known && st.ServiceRunning && !st.Busy && st.LiveTerminals == 0 {
		if replaced, replaceErr := d.releaseStaleSession(ctx, st, session); replaceErr != nil {
			diagnostics.Log("warn", "desktop-local", "could not restart controller from previous desktop session: "+replaceErr.Error())
		} else if replaced {
			st.ServiceRunning = false
			st.ControllerID = ""
		}
	}
	if d.adoptIdleStandalone && st.Known && st.ServiceRunning && !st.Busy && !st.Running && st.LiveTerminals == 0 {
		if adopted, adoptErr := d.releaseIdleStandalone(ctx, st); adoptErr != nil {
			diagnostics.Log("warn", "desktop-local", "could not restart idle non-desktop controller under desktop app: "+adoptErr.Error())
		} else if adopted {
			st.ServiceRunning = false
			st.ControllerID = ""
		}
	}
	bootstrap, cancel := context.WithTimeout(ctx, localBootstrapTimeout)
	defer cancel()
	var launchID string
	if !st.ServiceRunning {
		launchID = randomToken()
		c, started, err := startControllerExecutable(bootstrap, d.config, d.executable, "CAGE_DESKTOP_START_ID="+launchID)
		if started {
			defer func() {
				if returned != nil {
					d.releaseFailedLaunch(launchID)
				}
			}()
		} else {
			launchID = ""
		}
		if err != nil {
			return nil, err
		}
		c.close()
	}
	for {
		c, err := openController(d.profile)
		if err == nil {
			view, probeErr := c.localControl(bootstrap, "status", desktopLocalRequest{})
			if probeErr == nil {
				defer c.close()
				if st.ControllerID != "" && view.ControllerID != st.ControllerID {
					return nil, errors.New("本机后台已变化，请重新选择")
				}
				life := ctx
				if launchID != "" && view.LaunchID == launchID {
					life = context.WithValue(ctx, localLaunchKey{}, launchID)
				}
				session, err := d.localSession(life, bootstrap, c, view)
				if err == nil && launchID != "" && view.LaunchID == launchID {
					d.mu.Lock()
					d.ownedLaunch = launchID
					d.mu.Unlock()
				}
				return session, err
			}
			c.close()
			if launchID == "" {
				return nil, probeErr
			}
		} else if launchID == "" && !errors.Is(err, errControllerUnavailable) {
			return nil, err
		}
		select {
		case <-bootstrap.Done():
			return nil, errors.New("无法连接本机控制服务，请检查本机状态")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

type localLaunchKey struct{}

// Automatic release is limited to a controller started by this gateway.
// A standalone cage serve may be idle between requests and still have other callers.
func (d *desktopLocal) ReleaseOwnedIdle(ctx context.Context, expected DesktopLocalStatus) (bool, error) {
	d.mu.Lock()
	id := d.ownedLaunch
	d.mu.Unlock()
	if id == "" {
		return false, nil
	}
	c, err := openController(d.profile)
	if err != nil {
		return false, err
	}
	defer c.close()
	view, err := c.localControl(ctx, "status", desktopLocalRequest{})
	if err != nil {
		return false, err
	}
	if view.LaunchID != id {
		return false, nil
	}
	if !expected.Known || !view.Known || view.ControllerID != expected.ControllerID || view.Busy || view.Running || view.LiveTerminals > 0 {
		return false, errors.New("本机后台或任务状态已变化，请重新确认")
	}
	if err := d.Release(ctx, expected, false); err != nil {
		return false, err
	}
	return true, nil
}

// releaseStaleSession hands off a controller that survived an earlier desktop
// login session (for example after a macOS WindowServer/logout restart). A
// running guest VM is preserved so the replacement controller started inside
// the active session can reattach with a valid system XPC/bootstrap namespace.
func (d *desktopLocal) releaseStaleSession(ctx context.Context, expected DesktopLocalStatus, currentSession string) (bool, error) {
	if currentSession == "" {
		return false, nil
	}
	c, err := openController(d.profile)
	if errors.Is(err, errControllerUnavailable) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer c.close()
	view, err := c.localControl(ctx, "status", desktopLocalRequest{})
	if err != nil {
		return false, err
	}
	if view.SessionID == "" || view.SessionID == currentSession {
		return false, nil
	}
	if !view.Known || view.Busy || view.LiveTerminals > 0 {
		diagnostics.Log("warn", "desktop-local", "controller belongs to previous desktop session ("+view.SessionID+" != "+currentSession+") but has active work; keeping existing process")
		return false, nil
	}
	if expected.ControllerID != "" && view.ControllerID != expected.ControllerID {
		return false, nil
	}
	diagnostics.Log("info", "desktop-local", "restarting controller from previous desktop session ("+view.SessionID+" -> "+currentSession+")")
	releaseCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err = c.localControl(releaseCtx, "release", desktopLocalRequest{
		ControllerID: view.ControllerID,
		PreserveVM:   view.Running,
		Running:      view.Running,
		Terminals:    view.LiveTerminals,
	})
	if err != nil {
		return false, err
	}
	if err = c.waitForExit(); err != nil {
		return false, err
	}
	for processExists(c.record.PID) {
		select {
		case <-releaseCtx.Done():
			return false, errors.New("本机控制进程仍未退出")
		case <-time.After(75 * time.Millisecond):
		}
	}
	if _, err := os.Lstat(filepath.Join(d.profile, "control.json")); !os.IsNotExist(err) || !controllerLockFree(d.profile) {
		return false, errors.New("本机控制服务状态已变化，请重新确认")
	}
	return true, nil
}

// releaseIdleStandalone releases an idle controller that was started outside
// the desktop app (empty LaunchID, e.g. by CLI or an agent), so Connect can
// start a fresh controller under the desktop app's process hierarchy (required
// on macOS for host plugins to inherit Cage.app's TCC permissions). Active or
// desktop-launched controllers are never disturbed.
func (d *desktopLocal) releaseIdleStandalone(ctx context.Context, expected DesktopLocalStatus) (bool, error) {
	c, err := openController(d.profile)
	if errors.Is(err, errControllerUnavailable) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	view, err := c.localControl(ctx, "status", desktopLocalRequest{})
	c.close()
	if err != nil {
		return false, err
	}
	if view.LaunchID != "" {
		return false, nil
	}
	if !view.Known || view.Busy || view.Running || view.LiveTerminals > 0 {
		return false, nil
	}
	if expected.ControllerID != "" && view.ControllerID != expected.ControllerID {
		return false, nil
	}
	diagnostics.Log("info", "desktop-local", "restarting idle non-desktop controller under desktop app")
	if err := d.Release(ctx, view.DesktopLocalStatus, false); err != nil {
		return false, err
	}
	return true, nil
}

// ReleaseUnused handles a prepared local view that the user never entered.
// A previous controller, including an idle one, belongs to its original owner.
func (d *desktopLocal) ReleaseUnused(ctx context.Context, session *remotehost.Session) error {
	if session == nil || session.Context == nil {
		return nil
	}
	id, _ := session.Context.Value(localLaunchKey{}).(string)
	if id == "" {
		return nil
	}
	c, err := openController(d.profile)
	if errors.Is(err, errControllerUnavailable) {
		return nil
	}
	if err != nil {
		return err
	}
	defer c.close()
	view, err := c.localControl(ctx, "status", desktopLocalRequest{})
	if err != nil {
		return err
	}
	if view.LaunchID != id || !view.Known || view.Busy || view.Running || view.LiveTerminals > 0 {
		return nil
	}
	return d.Release(ctx, view.DesktopLocalStatus, false)
}

// The process started for a cancelled handshake may publish its record after
// cancellation. Only hand off a controller bearing this launcher's own nonce.
func (d *desktopLocal) releaseFailedLaunch(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), localBootstrapTimeout)
	defer cancel()
	for ctx.Err() == nil {
		c, err := openController(d.profile)
		if err == nil {
			view, err := c.localControl(ctx, "status", desktopLocalRequest{})
			c.close()
			if err == nil {
				if view.LaunchID == id && view.Known && !view.Busy && !view.Running && view.LiveTerminals == 0 {
					_ = d.Release(ctx, view.DesktopLocalStatus, false)
				}
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(150 * time.Millisecond):
		}
	}
}

func (d *desktopLocal) localSession(life, bootstrap context.Context, c *controllerClient, view desktopLocalReply) (*remotehost.Session, error) {
	var launch launchResponse
	status, err := c.callContext(bootstrap, "POST", "/api/launch", map[string]string{"token": c.record.Token}, &launch)
	if err != nil || status != http.StatusOK || launch.Controller != view.Identity {
		return nil, errors.New("本机后台身份已变化，请重新选择")
	}
	u, err := url.Parse(launch.URL)
	if err != nil || u.Scheme+"://"+u.Host != c.origin || u.User != nil || u.Fragment == "" {
		return nil, errors.New("本机连接入口无效")
	}
	status, err = c.callContext(bootstrap, "POST", "/api/session", map[string]string{"token": u.Fragment}, nil)
	if err != nil || status != http.StatusOK {
		return nil, errors.New("本机会话认证失败")
	}
	var m RemoteEntry
	status, err = c.callContext(bootstrap, "GET", "/api/remote/manifest", nil, &m)
	if err != nil || status != http.StatusOK || m.InstanceID != view.InstanceID || m.APIProtocol != remoteAPIProtocol || m.Origin != c.origin || m.Platform != runtime.GOOS {
		return nil, errors.New("本机工作台版本或环境身份不符")
	}
	manifest := remotehost.PublicManifest{APIProtocol: m.APIProtocol, InstanceID: m.InstanceID, Version: m.Version, Platform: m.Platform, Runtime: m.Runtime, Capabilities: m.Capabilities}
	c.client.Jar.SetCookies(u, []*http.Cookie{{
		Name:  cookieName + "_" + u.Port() + "-desktop",
		Value: trustedViewProof(c.record.Token), Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
	}})
	return remotehost.LocalSession(life, c.origin, manifest, c.client.Jar)
}

func (d *desktopLocal) Release(ctx context.Context, expected DesktopLocalStatus, stop bool) error {
	if !expected.Known || !expected.ServiceRunning || expected.ControllerID == "" {
		return errors.New("请先确认本机控制服务身份")
	}
	c, err := openController(d.profile)
	if err != nil {
		return errors.New("本机控制服务已变化，请刷新后重试")
	}
	defer c.close()
	return c.releaseLocal(ctx, expected, stop)
}

func (c *controllerClient) releaseLocal(ctx context.Context, expected DesktopLocalStatus, stop bool) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	request := desktopLocalRequest{ControllerID: expected.ControllerID, Stop: stop, Running: expected.Running, Terminals: expected.LiveTerminals}
	reply, err := c.localControl(ctx, "release", request)
	if err != nil {
		return err
	}
	if reply.JobRunning {
		jobID := reply.JobID
		if jobID == "" || !stop {
			return errors.New("本机停止作业身份无效")
		}
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(150 * time.Millisecond):
			}
			view, err := c.localControl(ctx, "status", desktopLocalRequest{})
			if err != nil {
				return err
			}
			if view.JobID != jobID || view.JobFailed {
				return errors.New("本机停止作业已变化或失败，请回到本机确认状态")
			}
			if view.JobRunning {
				continue
			}
			if !view.Known || view.Busy || view.Running || view.LiveTerminals != 0 {
				return errors.New("本机工作尚未完全停止")
			}
			_, err = c.localControl(ctx, "release", desktopLocalRequest{ControllerID: view.ControllerID, JobID: jobID})
			if err != nil {
				return err
			}
			break
		}
	}
	if err = c.waitForExit(); err != nil {
		return err
	}
	for processExists(c.record.PID) {
		select {
		case <-ctx.Done():
			return errors.New("本机控制进程仍未退出")
		case <-time.After(75 * time.Millisecond):
		}
	}
	// Unlike a cold selector probe, this explicit release may query an existing
	// local container engine to verify that its workload actually stopped.
	if _, err := os.Lstat(filepath.Join(c.profile, "control.json")); !os.IsNotExist(err) || !controllerLockFree(c.profile) {
		return errors.New("本机控制服务状态已变化，请重新确认")
	}
	if running, err := controllerVMState(c.profile); err != nil || running || managedproxy.ConfirmStopped(ctx, c.profile) != nil {
		return errors.New("本机资源状态待确认")
	}
	return nil
}
