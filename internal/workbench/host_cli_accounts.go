package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
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
	if strings.TrimSpace(selector) == "" {
		return "", errors.New("请提供 cage account list 中的账号编号")
	}
	// IDs and human-readable numbers remain stable across renames. Accept
	// the zero-padded number shown by the desktop as well as the CLI spelling.
	number, numberErr := strconv.ParseUint(strings.TrimPrefix(selector, "#"), 10, 32)
	for _, a := range list.Accounts {
		if a.ID == selector || numberErr == nil && number > 0 && uint64(a.Number) == number {
			return a.ID, nil
		}
	}
	if id, err := list.byCLIName(selector); err == nil {
		return id, nil
	}
	// Explicit account subcommands also accept an email or an old display label.
	var id string
	for _, a := range list.Accounts {
		if !strings.EqualFold(a.Email, selector) && a.Label != selector {
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

func namedCLIAccount(list claudeAccountList, name, expectedID string) (claudeSelection, error) {
	id, err := list.byCLIName(name)
	if err != nil {
		return claudeSelection{}, err
	}
	if expectedID != "" && expectedID != id {
		return claudeSelection{}, errors.New("这个名称对应的账号已变化，请核对 cage account list 后重试")
	}
	return list.explicitCodeAccount(id)
}

func (s *Server) renameHostCLIAccount(ctx context.Context, selector, name string) error {
	s.claudeAccounts.mu.Lock()
	defer s.claudeAccounts.mu.Unlock()
	list, err := s.loadClaudeAccounts()
	if err != nil {
		return err
	}
	id, err := resolveCLIAccount(list, selector)
	if err != nil {
		return err
	}
	if err = list.renameCLIAccount(id, name); err != nil {
		return err
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	if !s.mobileAuthorized(ctx) {
		return errMobileLocked
	}
	return s.saveClaudeAccounts(list)
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
	if in.Action == "rename-account" {
		if err := s.renameHostCLIAccount(ctx, in.Account, in.Name); err != nil {
			return err
		}
		return c.event(hostCLIEvent{Type: "exit", Message: "Cage 账号名称已保存。"})
	}
	list, err := s.cliAccountRoster()
	if err != nil {
		return err
	}
	if in.Action == "resolve-name" {
		account, err := namedCLIAccount(list, in.Name, "")
		if err != nil {
			return err
		}
		if _, err = io.WriteString(hostCLIOutput{c, 'o'}, account.ID); err != nil {
			return err
		}
		return c.event(hostCLIEvent{Type: "exit"})
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
	table := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	if len(list.Accounts) > 0 {
		fmt.Fprintln(table, "#\tID\tNAME\tEMAIL\tSTATUS")
	}
	for _, a := range list.Accounts {
		state := a.Status
		if a.CodeUnlinked {
			state = "需授权"
		}
		mark := ""
		if a.ID == pin {
			mark = " [CLI]"
		}
		fmt.Fprintf(table, "#%d\t%s\t%s\t%s\t%s%s\n", a.Number, a.ID, a.Name, a.Email, state, mark)
	}
	table.Flush()
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
