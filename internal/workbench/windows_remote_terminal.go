package workbench

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/sylvinus/agent-vm/internal/guestos"
)

// The remote adapter starts only a fixed home PowerShell in the paired ordinary
// account's AppContainer. It does not expose an administrative host terminal.
func (s *Server) createRemoteWindowsTerminal(input terminalRequest) (*terminalSession, error) {
	if input.Kind == "claude" || input.Kind == "claude-login" {
		return s.createWindowsCLITerminal(input)
	}
	if input.Kind != "" && input.Kind != "shell" || input.ClaudeAccountID != "" || input.sharedID != "" {
		return nil, errors.New("普通 PowerShell 终端不能指定 Claude 账号、会话或其他终端类型")
	}
	s.mu.Lock()
	ready, closing := s.ready, s.closing
	s.mu.Unlock()
	if closing {
		return nil, errTerminalsClosed
	}
	if !ready {
		return nil, errTerminalNotReady
	}
	if s.busy() {
		return nil, errTerminalBusy
	}
	if err := s.terminals.canStart(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(s.lifetimeContext(), 10*time.Second)
	defer cancel()
	state, err := s.windowsStatus(ctx)
	if err != nil {
		return nil, err
	}
	if input.Workspace != "" && !strings.EqualFold(input.Workspace, state.Home) {
		return nil, errors.New("Windows 云端目前仅开放沙箱工作目录，外部项目尚未授权")
	}
	cmd, err := guestos.WindowsRelayCommand(s.lifetimeContext(), s.profile, "headless-terminal")
	if err != nil {
		return nil, err
	}
	// Keep the existing account epoch check for mobile callers as well.
	if input.requestContext != nil {
		s.account.mu.Lock()
		defer s.account.mu.Unlock()
		if !s.mobileAuthorized(input.requestContext) {
			return nil, errMobileLocked
		}
	}
	return s.terminals.startSessionWithPTY(cmd, state.Home, "shell", claudeSelection{}, nil,
		func() (terminalPTY, error) { return startWindowsGuestTerminal(cmd, 100, 30) })
}

// Remote sessions can use the package's own home, never an arbitrary host path.
func (s *Server) remoteWindowsCLIWorkspace(path string) (string, error) {
	s.mu.Lock()
	home := s.windowsState.Home
	s.mu.Unlock()
	if home == "" || path != "" && !strings.EqualFold(path, home) {
		return "", errors.New("Windows 云端目前仅开放沙箱工作目录，外部项目尚未授权")
	}
	return home, nil
}
