package workbench

import (
	"errors"
	"strconv"
	"strings"

	"github.com/sylvinus/agent-vm/internal/cagecmd"
)

// A name selects a registry ID. Sessions, guest directories and identity
// checks continue to use that ID. Legacy names are computed without a write;
// the next normal registry save records them alongside the stable numbers.
func (list *claudeAccountList) prepareCLIAccounts() error {
	if err := list.assignNumbers(); err != nil {
		return err
	}
	claimed := make(map[string]int, len(list.Accounts))
	for _, a := range list.Accounts {
		if cagecmd.ValidAccountName(a.Name) {
			claimed[cagecmd.AccountNameKey(a.Name)]++
		}
	}
	if len(claimed) == len(list.Accounts) {
		return nil
	}
	var pending []int
	for i := range list.Accounts {
		a := &list.Accounts[i]
		if a.Name != "" && claimed[cagecmd.AccountNameKey(a.Name)] == 1 && cagecmd.ValidAccountName(a.Name) {
			continue
		}
		a.Name = ""
		pending = append(pending, i)
	}
	list.assignSuggestedNames(claimed, func(a claudeAccount) string { return a.Label })
	list.assignSuggestedNames(claimed, func(a claudeAccount) string {
		// Email in the registry comes from an authenticated client observation.
		email := (claudeGuestSnapshot{Email: a.Email, LoggedIn: true}).email()
		local, _, _ := strings.Cut(email, "@")
		return local
	})
	for _, i := range pending {
		a := &list.Accounts[i]
		if a.Name != "" {
			continue
		}
		base := "account-" + strconv.FormatUint(uint64(a.Number), 10)
		name := base
		for suffix := 2; claimed[cagecmd.AccountNameKey(name)] != 0 || !cagecmd.ValidAccountName(name); suffix++ {
			name = base + "-" + strconv.Itoa(suffix)
		}
		a.Name = name
		claimed[cagecmd.AccountNameKey(name)] = 1
	}
	return nil
}

func (list *claudeAccountList) assignSuggestedNames(claimed map[string]int, suggest func(claudeAccount) string) {
	type suggestion struct {
		index int
		name  string
	}
	unique := make(map[string]suggestion)
	for i, a := range list.Accounts {
		name := suggest(a)
		if !cagecmd.ValidAccountName(name) {
			continue
		}
		key := cagecmd.AccountNameKey(name)
		claimed[key]++
		if claimed[key] == 1 && a.Name == "" {
			unique[key] = suggestion{i, name}
		} else {
			// Neither claimant gets the bare name, even if a later email would
			// suggest it again. Saved order must not choose an identity.
			delete(unique, key)
		}
	}
	for _, candidate := range unique {
		list.Accounts[candidate.index].Name = candidate.name
	}
}

func (list claudeAccountList) byCLIName(name string) (string, error) {
	if !cagecmd.ValidAccountName(name) {
		return "", errors.New("Claude 账号命令名无效，请运行 cage account list 查看")
	}
	key := cagecmd.AccountNameKey(name)
	var id string
	for _, a := range list.Accounts {
		if a.Name == "" || cagecmd.AccountNameKey(a.Name) != key {
			continue
		}
		if id != "" {
			return "", errors.New("Claude 账号命令名重复，请使用账号编号改名")
		}
		id = a.ID
	}
	if id == "" {
		return "", errors.New("找不到这个 Cage 账号名称。运行 cage account list 查看名称，或使用 cage claude 传入提示词和 Claude 子命令。")
	}
	return id, nil
}

func (list *claudeAccountList) renameCLIAccount(id, name string) error {
	a := list.find(id)
	if a == nil {
		return errors.New("未找到 Claude 账号，请运行 cage account list 查看")
	}
	if !cagecmd.ValidAccountName(name) {
		return errors.New("账号命令名须以字母或数字开头，最多 64 个字符，可含 -、_ 和 .。不能使用纯数字、内部 ID 或保留命令。")
	}
	key := cagecmd.AccountNameKey(name)
	for _, other := range list.Accounts {
		if other.ID != id && other.Name != "" && cagecmd.AccountNameKey(other.Name) == key {
			return errors.New("该账号命令名已被使用，请选择其他名称")
		}
	}
	a.Name = name
	return nil
}
