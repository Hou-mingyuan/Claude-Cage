package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (s *Server) cliPinnedAccount() (string, error) {
	data, err := os.ReadFile(filepath.Join(s.profile, "host-cli-account.json"))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var record struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(data, &record) != nil || record.ID != "" && !validClaudeID(record.ID) {
		return "", errors.New("CLI 账号设置无效，可用 cage account use auto 恢复自动分配")
	}
	return record.ID, nil
}

func resolveCLIAccount(list claudeAccountList, selector string) (string, error) {
	if selector == "" {
		return "", errors.New("请提供 cage account list 中的账号编号")
	}
	var id string
	for _, a := range list.Accounts {
		if a.ID != selector && strconv.FormatUint(uint64(a.Number), 10) != strings.TrimPrefix(selector, "#") && !strings.EqualFold(a.Email, selector) && a.Label != selector {
			continue
		}
		if id != "" {
			return "", errors.New("有多个同名账号，请使用 cage account list 中的编号")
		}
		id = a.ID
	}
	if id == "" {
		return "", errors.New("找不到该 Claude 账号，请运行 cage account list")
	}
	return id, nil
}

func (s *Server) cliAccountRoster() (claudeAccountList, error) {
	s.claudeAccounts.mu.Lock()
	defer s.claudeAccounts.mu.Unlock()
	return s.loadClaudeAccounts()
}

func (s *Server) saveCLIAccount(ctx context.Context, id string, onlyIfUnpinned bool) error {
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	if !s.mobileAuthorized(ctx) {
		return errMobileLocked
	}
	if onlyIfUnpinned {
		pin, err := s.cliPinnedAccount()
		if err != nil || pin != "" {
			return err
		}
	}
	return writePrivate(filepath.Join(s.profile, "host-cli-account.json"), map[string]string{"id": id})
}

func (s *Server) cliAccountCommand(ctx context.Context, c *hostCLIConn, in hostCLIRequest) error {
	list, err := s.cliAccountRoster()
	if err != nil {
		return err
	}
	if in.Action == "account-status" {
		count := 0
		for _, a := range list.Accounts {
			if a.Status == "ready" && !a.CodeUnlinked {
				count++
			}
		}
		if _, err := fmt.Fprintf(hostCLIOutput{c, 'o'}, "已登记且状态可用的 Claude Code 账号：%d\n", count); err != nil {
			return err
		}
		return c.event(hostCLIEvent{Type: "exit"})
	}
	if in.Action == "use-account" {
		id := ""
		if in.Account != "auto" {
			id, err = resolveCLIAccount(list, in.Account)
			if err != nil {
				return err
			}
			a := list.find(id)
			if a.CodeUnlinked || a.Status != "ready" {
				return errors.New("所选 Claude Code 账号尚未登录")
			}
		}
		if err = s.saveCLIAccount(ctx, id, false); err != nil {
			return err
		}
		return c.event(hostCLIEvent{Type: "exit", Message: "CLI 账号选择已保存。"})
	}
	pin, err := s.cliPinnedAccount()
	if err != nil {
		return err
	}
	var b strings.Builder
	if pin == "" {
		b.WriteString("CLI 使用工作台的账号分配规则。\n")
	}
	if len(list.Accounts) == 0 {
		b.WriteString("尚未登记 Claude 账号，可运行 cage login。\n")
	}
	for _, a := range list.Accounts {
		label := a.Label
		if label == "" {
			label = a.Email
		}
		state := a.Status
		if a.CodeUnlinked {
			state = "需授权"
		}
		mark := ""
		if a.ID == pin {
			mark = " [CLI]"
		}
		fmt.Fprintf(&b, "#%d\t%s\t%s%s\n", a.Number, label, state, mark)
	}
	if _, err = (hostCLIOutput{c, 'o'}).Write([]byte(b.String())); err != nil {
		return err
	}
	return c.event(hostCLIEvent{Type: "exit"})
}

func (s *Server) cliDefaultAccount(project string) (claudeSelection, error) {
	id, err := s.cliPinnedAccount()
	if err != nil {
		return claudeSelection{}, err
	}
	selection, err := s.selectClaudeAccount(id, false, project)
	if id != "" || !errors.Is(err, errNoClaudeAllocation) {
		return selection, err
	}
	// A single account registered by the desktop is not automatically
	// enrolled in scheduling. It is still the user's only CLI identity.
	list, readErr := s.cliAccountRoster()
	if readErr != nil {
		return selection, readErr
	}
	if len(list.Accounts) == 1 {
		a := list.Accounts[0]
		if !a.Enabled && a.Status == "ready" && !a.CodeUnlinked {
			return s.selectClaudeAccount(a.ID, false, project)
		}
	}
	return selection, err
}

// A login the CLI user just completed can be used explicitly without
// enrolling that account in the desktop's automatic scheduling pool.
func (s *Server) bindCLILogin(ctx context.Context, subject string) error {
	list, err := s.cliAccountRoster()
	if err != nil {
		return err
	}
	a := list.bySubject(subject)
	if a == nil || a.CodeUnlinked || a.Status != "ready" {
		return errors.New("Claude 已授权，但该身份尚未登记为可用的 Code 账号")
	}
	for _, entry := range list.Accounts {
		if entry.Enabled && entry.Status == "ready" && !entry.CodeUnlinked {
			return nil
		}
	}
	return s.saveCLIAccount(ctx, a.ID, true)
}
