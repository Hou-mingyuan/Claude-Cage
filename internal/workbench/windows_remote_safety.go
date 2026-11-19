package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/guestos"
)

type windowsRemoteEvidence struct {
	SandboxSID     string         `json:"sandbox_sid"`
	ProxyPort      int            `json:"proxy_port"`
	Session        string         `json:"session"`
	TokenVerified  bool           `json:"token_verified"`
	PolicyVerified bool           `json:"-"` // Set only by the host's WFP readback.
	Nonce          string         `json:"nonce"`
	TCP            map[string]int `json:"tcp"`
	UDP4           *int           `json:"udp4"`
	UDP6           *int           `json:"udp6"`
	SystemDNS      *int           `json:"system_dns"`
	PrivateRead    *int           `json:"private_read"`
	HomeRoundtrip  bool           `json:"home_roundtrip"`
}

func (s *Server) remoteWindowsObservation(ctx context.Context, input map[string]any) (guestObservation, error) {
	var observation guestObservation
	cfg, err := loadConfig(s.configPath)
	if err != nil {
		return observation, err
	}
	input["timezone"] = cfg.Timezone
	cmd, err := guestos.WindowsRelayCommand(ctx, s.profile, "headless-safety")
	if err != nil {
		return observation, err
	}
	data, err := json.Marshal(input)
	if err != nil {
		return observation, err
	}
	cmd.Stdin = bytes.NewReader(append(data, '\n'))
	cmd.Stderr = io.Discard
	var output bytes.Buffer
	cmd.Stdout = &boundedWindowsStatusWriter{buffer: &output, remaining: 64 << 10}
	if err = cmd.Run(); err != nil {
		return observation, errors.New("Windows 云端沙箱观测未完成，外网保持关闭")
	}
	observation, err = decodeWindowsObservation(output.Bytes())
	if err != nil {
		return observation, err
	}
	evidence := observation.Remote
	if evidence == nil || !evidence.TokenVerified || !strings.HasPrefix(evidence.SandboxSID, "S-1-15-2-") || !strings.HasPrefix(evidence.Session, "headless-") {
		return observation, errors.New("Windows 云端观测未确认实际 AppContainer 身份")
	}
	// This command reads all four account filters, all four package filters,
	// the exact broker permit and loopback exception; it cannot install rules.
	policy := exec.CommandContext(ctx, s.windowsGuest.RelayExecutable, "remote-network-policy", "broker-status", "--profile", guestos.WindowsProfilePath(s.profile))
	output.Reset()
	policy.Stdout = &boundedWindowsStatusWriter{buffer: &output, remaining: 8192}
	policy.Stderr = io.Discard
	var state struct {
		Version    int    `json:"version"`
		Package    string `json:"package"`
		Broker     string `json:"broker"`
		Filter     bool   `json:"broker_filter"`
		Loopback   bool   `json:"loopback_exemption"`
		UDPFilters int    `json:"udp_bind_filters"`
	}
	if policy.Run() != nil || json.Unmarshal(output.Bytes(), &state) != nil || state.Version != 1 || state.Package != evidence.SandboxSID || state.Broker != "127.0.0.1:17897" || !state.Filter || !state.Loopback || state.UDPFilters != 2 {
		return observation, errors.New("Windows 云端 WFP 策略与当前沙箱不匹配，外网保持关闭")
	}
	evidence.PolicyVerified = true
	return observation, nil
}

func remoteWindowsDenied(e *windowsRemoteEvidence, names ...string) bool {
	if e == nil || !e.TokenVerified || !e.PolicyVerified {
		return false
	}
	for _, name := range names {
		code, found := e.TCP[name]
		if !found || code != 10013 {
			return false
		} // WSAEACCES, not a timeout.
	}
	return true
}

func remoteWindowsDNSBlocked(v guestObservation) bool {
	e := v.Remote
	return remoteWindowsDenied(e, "dns", "dot") && e.UDP4 != nil && *e.UDP4 == 10013 &&
		e.SystemDNS != nil && (*e.SystemDNS == 11001 || *e.SystemDNS == 10013)
}

func (s *Server) remoteWindowsBoundary(ctx context.Context, cfg cage.Config, client *cage.GateClient,
	set func(string, cage.CheckState, string, string)) (guestObservation, cage.GateStatus, error) {
	var state cage.GateStatus
	var observation guestObservation
	private, err := os.CreateTemp(s.profile, ".cage-private-probe-*")
	if err != nil {
		return observation, state, err
	}
	private.Close()
	defer os.Remove(private.Name())
	nonce := randomToken()
	control, err := url.Parse(s.origin)
	if err != nil {
		return observation, state, err
	}
	port, _ := strconv.Atoi(control.Port())
	_, proxyPort := guestProxyEndpoint(cfg, "127.0.0.1")
	if proxyPort < 1 || proxyPort > 65535 {
		return observation, state, errors.New("无法确定当前代理端口，外网保持关闭")
	}
	s.setMessage("正在读取云端 AppContainer、WFP 与实际目录权限…")
	observation, err = s.remoteWindowsObservation(ctx, map[string]any{"stage": "boundary", "nonce": nonce, "private_file": private.Name(), "control_port": port, "proxy_port": proxyPort})
	if err != nil {
		return observation, state, err
	}
	e := observation.Remote
	if e.Nonce != nonce {
		return observation, state, errors.New("Windows 云端观测不属于本次检测")
	}
	state, err = client.Status(ctx)
	if err != nil {
		return observation, state, err
	}
	result := func(id string, ok bool, evidence, remedy string) {
		status := cage.CheckFailed
		if ok {
			status = cage.CheckPassed
			remedy = ""
		}
		set(id, status, evidence, remedy)
	}
	settings := windowsGuestSettingsMatch(observation, cfg)
	result("guest_settings", settings,
		fmt.Sprintf("实际沙箱进程：时区=%s、语言=%s；Windows 系统：时区=%s、语言=%s；已保存期望=%s/%s", observation.TZ, observation.ProcessLocale, observation.SystemTimezone, observation.SystemLocale, cfg.Timezone, cfg.Locale),
		"云端系统/终端配置尚未与已保存设置一致；浏览器配置不能替代系统观测")
	result("egress_policy", state.PolicyVerified && state.Configuration == cfg.Fingerprint() && remoteWindowsDenied(e, "public"),
		fmt.Sprintf("实际无网络 capability 的 AppContainer；WFP 账户与 package 规则和唯一受控代理 permit 已读回；公网 TCP 返回 Winsock %d", e.TCP["public"]), "恢复当前安装的 WFP 策略后重检")
	// Older probes always tested 17896 and did not return the port.
	testedPort := e.ProxyPort
	if testedPort == 0 {
		testedPort = 17896
	}
	requiresSession := s.managementRequiresSession(ctx)
	result("management_boundary", testedPort == proxyPort && requiresSession && remoteWindowsDenied(e, "management", "upstream", "metadata", "loopback6", "metadata6"),
		fmt.Sprintf("直连错误码：控制器=%d、上游代理=%d（检测端口 %d，当前端口 %d）、元数据=%d、IPv6 回环=%d、IPv6 元数据=%d；管理接口拒绝匿名访问=%t", e.TCP["management"], e.TCP["upstream"], testedPort, proxyPort, e.TCP["metadata"], e.TCP["loopback6"], e.TCP["metadata6"], requiresSession), "检查云端管理接口与 package 的 WFP 隔离")
	result("udp_policy", e.PolicyVerified && e.UDP4 != nil && *e.UDP4 == 10013 && e.UDP6 != nil && *e.UDP6 == 10013,
		fmt.Sprintf("沙箱 UDP/IPv4 错误码=%v、UDP/IPv6 错误码=%v；只将 Winsock 10013 作为拒绝证据，不使用无响应推断", codeValue(e.UDP4), codeValue(e.UDP6)), "恢复 package IPv4/IPv6 的 WFP 拒绝规则")
	result("ipv6_policy", remoteWindowsDenied(e, "ipv6", "loopback6", "metadata6") && e.UDP6 != nil && *e.UDP6 == 10013,
		fmt.Sprintf("沙箱 IPv6 公网/回环/元数据 TCP 错误码=%d/%d/%d，UDP=%v；已核对 WFP connect/receive v6 规则", e.TCP["ipv6"], e.TCP["loopback6"], e.TCP["metadata6"], codeValue(e.UDP6)), "恢复 package IPv6 的 WFP 拒绝规则")
	projects, projectErr := s.projectPaths()
	s.mu.Lock()
	noShares := len(s.projectShares) == 0
	s.mu.Unlock()
	result("filesystem_boundary", projectErr == nil && len(projects) == 0 && noShares && e.PrivateRead != nil && *e.PrivateRead == 5 && e.HomeRoundtrip,
		fmt.Sprintf("沙箱 home 读写往返=%t；实际存在的控制器私有文件读取错误码=%v；外部项目数=%d，未建立共享=%t", e.HomeRoundtrip, codeValue(e.PrivateRead), len(projects), noShares), "当前只支持沙箱 home；外部项目必须完成独立授权接线")
	return observation, state, nil
}

func codeValue(code *int) any {
	if code == nil {
		return "未返回"
	}
	return *code
}
