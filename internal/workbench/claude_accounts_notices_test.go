package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeAccountNoticesInPanelView(t *testing.T) {
	s := testServer(t)
	s.claudeAccounts.probe = func(context.Context, string) (claudeGuestSnapshot, error) {
		return claudeGuestSnapshot{}, nil
	}
	managed := strings.Repeat("a", 24)
	quiet := strings.Repeat("b", 24)
	list := claudeAccountList{Accounts: []claudeAccount{
		{ID: managed, Source: "managed", Email: "a@example.org", Enabled: true, Status: "ready"},
		{ID: quiet, Source: "managed", Email: "b@example.org", Enabled: true, Status: "ready"},
	}}
	if err := s.saveClaudeAccounts(list); err != nil {
		t.Fatal(err)
	}
	s.ready = true
	calls := 0
	var asked []string
	s.claudeAccounts.noticeProbe = func(_ context.Context, ids []string) (map[string][]claudeAccountNotice, error) {
		calls++
		asked = ids
		return map[string][]claudeAccountNotice{
			managed: {{ID: "0123456789abcdef", Kind: "resource_conflict", Item: "CLAUDE.md", Local: "~/x/CLAUDE.md", Shared: "~/.claude/CLAUDE.md", Since: 1, Message: "冲突"}},
		}, nil
	}
	v, _, err := s.claudePanelView(context.Background())
	if err != nil || len(v.Accounts) != 2 || len(v.Accounts[0].Notices) != 1 || v.Accounts[0].Notices[0].Kind != "resource_conflict" || v.Accounts[1].Notices != nil {
		t.Fatal(v, err)
	}
	if strings.Join(asked, " ") != managed+" "+quiet {
		t.Fatal(asked)
	}
	// Cached within the freshness window.
	if _, _, err = s.claudePanelView(context.Background()); err != nil || calls != 1 {
		t.Fatal(calls, err)
	}
	// Runtime notices never reach the saved registry.
	data, err := os.ReadFile(filepath.Join(s.profile, "claude-accounts.json"))
	if err != nil || strings.Contains(string(data), "notices") {
		t.Fatal(string(data), err)
	}
	encoded, _ := json.Marshal(v)
	if !strings.Contains(string(encoded), `"notices":[{"id":"0123456789abcdef","kind":"resource_conflict"`) {
		t.Fatal(string(encoded))
	}
	// A helper failure (for example an older guest without the mode) means
	// no notices, never a failed account list.
	s.claudeAccounts.noticesAt = s.claudeAccounts.noticesAt.Add(-claudeNoticesFresh)
	s.claudeAccounts.noticeProbe = func(context.Context, []string) (map[string][]claudeAccountNotice, error) {
		return nil, errors.New("usage: cage-claude-account")
	}
	v, _, err = s.claudePanelView(context.Background())
	if err != nil || len(v.Accounts) != 2 || v.Accounts[0].Notices != nil {
		t.Fatal(v, err)
	}
}

func TestClaudeAccountNoticesSkippedWhenOffline(t *testing.T) {
	s := testServer(t)
	if err := s.saveClaudeAccounts(claudeAccountList{Accounts: []claudeAccount{{ID: strings.Repeat("c", 24), Source: "managed", Status: "ready"}}}); err != nil {
		t.Fatal(err)
	}
	s.claudeAccounts.noticeProbe = func(context.Context, []string) (map[string][]claudeAccountNotice, error) {
		t.Fatal("guest read while environment is offline")
		return nil, nil
	}
	if v, _, err := s.claudePanelView(context.Background()); err != nil || len(v.Accounts) != 1 {
		t.Fatal(v, err)
	}
}

func TestCleanClaudeNotices(t *testing.T) {
	in := []claudeAccountNotice{
		{ID: "", Kind: "resource_conflict"},
		{ID: "1", Kind: "", Message: ""},
		{ID: "2", Kind: "future_kind", Message: "说明"},
		{ID: "3", Kind: "memory_merged", Item: strings.Repeat("项", 400)},
	}
	out := cleanClaudeNotices(in)
	if len(out) != 2 || out[0].ID != "2" || len(out[1].Item) > 512 || !strings.HasPrefix(strings.Repeat("项", 400), out[1].Item) {
		t.Fatal(out)
	}
	var many []claudeAccountNotice
	for range claudeNoticesLimit + 5 {
		many = append(many, claudeAccountNotice{ID: "x", Kind: "mcp_kept"})
	}
	if len(cleanClaudeNotices(many)) != claudeNoticesLimit {
		t.Fatal("notice count not bounded")
	}
	if validClaudeGuestID("default") != true || validClaudeGuestID("../x") || !validClaudeGuestID(strings.Repeat("d", 24)) {
		t.Fatal("guest id validation")
	}
}
