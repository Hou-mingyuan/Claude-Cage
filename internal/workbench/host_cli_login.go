package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type hostCLIFocus struct {
	ID    string `json:"id"`
	Tab   string `json:"tab,omitempty"`
	until time.Time
}

func (s *Server) requestCLIBrowser(tab string) {
	s.mu.Lock()
	s.hostCLIBrowser = hostCLIFocus{ID: randomToken()[:16], Tab: tab, until: time.Now().Add(5 * time.Minute)}
	s.mu.Unlock()
}

func (s *Server) hostCLILogin(ctx context.Context, c *hostCLIConn) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _, _, _ = c.ReadMessage(); cancel() }()
	selection, selectErr := s.cliDefaultAccount("")
	var loginID string
	if errors.Is(selectErr, errClaudeNeedsLogin) {
		id, err := s.cliPinnedAccount()
		if err != nil {
			return err
		}
		list, err := s.cliAccountRoster()
		if err != nil {
			return err
		}
		a := list.find(id)
		if a == nil {
			return errors.New("CLI 所选账号已移除，请用 cage account use auto 恢复自动分配")
		}
		if a.Source == "managed" {
			loginID = a.ID
		}
	} else if selectErr != nil && !errors.Is(selectErr, errNoClaudeAllocation) {
		return selectErr
	}
	if errors.Is(selectErr, errNoClaudeAllocation) {
		// The desktop can have observed a valid default login without the
		// user enrolling it in scheduling. Explicit login may claim it.
		reading, err := s.probeClaude(ctx, "default")
		if err != nil {
			return err
		}
		if reading.subject() != "" {
			selection, selectErr = s.verifiedCodeAccount(ctx, "default", "")
			if selectErr != nil {
				return selectErr
			}
			if selection.ID != "" {
				if err = s.bindCLILogin(ctx, selection.Subject); err != nil {
					return err
				}
			}
		}
	}
	if selectErr == nil {
		if selection.ID != "" && selection.GuestID != "default" {
			loginID = selection.ID
		}
		guest := selection.GuestID
		if guest == "" {
			guest = "default"
		}
		reading, err := s.probeCodeLogin(ctx, guest, selection.ID)
		if err != nil {
			return err
		}
		if reading.subject() != "" && (selection.Subject == "" || selection.Subject == reading.subject()) {
			if selection.ID == "" {
				selection, err = s.verifiedCodeAccount(ctx, "default", "")
				if err != nil {
					return err
				}
				if selection.ID != "" {
					if err = s.bindCLILogin(ctx, selection.Subject); err != nil {
						return err
					}
				}
			}
			s.mu.Lock()
			token := s.credentials.Token
			s.mu.Unlock()
			data := map[string]string{"url": "https://claude.ai"}
			if selection.ID != "" {
				a, err := s.accountForRoute(selection.ID)
				if err != nil {
					return err
				}
				if a.ProxyNodeID != "" {
					if err = s.ensureAccountNetwork(ctx, a.ID, a.ProxyNodeID); err != nil {
						return err
					}
					data["route"] = a.ID
				}
				data["account"] = a.ID
			}
			body, _ := json.Marshal(data)
			reply, err := s.guestAPI(ctx, token, "POST", "/new", body)
			if err != nil {
				return err
			}
			var tab struct{ ID string }
			if json.Unmarshal(reply, &tab) != nil || tab.ID == "" {
				return errors.New("Cage 浏览器未能打开 Claude 页面")
			}
			s.requestCLIBrowser(tab.ID)
			return c.event(hostCLIEvent{Type: "exit", Message: "Claude 账号已登录，可使用 Claude Code。"})
		}
	}
	t, err := s.createTerminal(terminalRequest{Kind: "claude-login", ClaudeAccountID: loginID, BackgroundLogin: true, requestContext: ctx})
	if err != nil {
		return err
	}
	s.requestCLIBrowser("")
	if err := c.event(hostCLIEvent{Type: "notice", Message: "请在 Cage 浏览器中完成 Claude 账号授权。"}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.done:
	}
	t.mu.Lock()
	success, guest, account := t.exitOK && !t.dismissed, t.loginGuestID, t.claudeAccountID
	t.mu.Unlock()
	if !success {
		return errors.New("Claude 账号授权未完成")
	}
	reading, err := s.probeCodeLogin(ctx, guest, account)
	if err != nil {
		return err
	}
	if reading.subject() == "" {
		return errors.New("Claude Code 尚未确认登录")
	}
	if err = s.confirmClaudeLogin(ctx, account); err != nil {
		return err
	}
	if err = s.bindCLILogin(ctx, reading.subject()); err != nil {
		return err
	}
	return c.event(hostCLIEvent{Type: "exit", Message: "Claude 账号已登录，可使用 Claude Code。"})
}
