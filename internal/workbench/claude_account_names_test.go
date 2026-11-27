package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestClaudeCLINamesMigrateWithoutWritingOrChoosingAnAmbiguousLabel(t *testing.T) {
	s := testServer(t)
	path := filepath.Join(s.profile, "claude-accounts.json")
	id := func(n int) string { return fmt.Sprintf("%024x", n) }
	legacy := []map[string]any{
		{"id": id(1), "number": 1, "label": "Sarah", "email": "sarah@work.example"},
		{"id": id(2), "number": 2, "label": "SARAH", "email": "sarah@home.example"},
		{"id": id(3), "number": 3, "label": "研究组", "email": "research@example.org"},
		{"id": id(4), "number": 4, "label": "STOP", "email": "Personal@example.org"},
		{"id": id(5), "number": 5, "label": "旧 备注", "email": "memo@example.org"},
		{"id": id(6), "number": 6, "label": "44", "email": "qa+work@example.org"},
		{"id": id(7), "number": 7, "name": "ACCOUNT-1", "label": "Existing", "email": "own@example.org"},
		{"id": id(8), "number": 8, "label": "account-1-2"},
		{"id": id(9), "label": "sarah", "email": "amy@example.org"},
	}
	data, err := json.Marshal(map[string]any{"accounts": legacy})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	want := []string{"account-1-3", "account-2", "研究组", "personal", "memo", "account-6", "ACCOUNT-1", "account-1-2", "amy"}
	check := func(list claudeAccountList) {
		t.Helper()
		for i, name := range want {
			a := list.find(id(i + 1))
			if a == nil || a.Name != name || a.Number != uint32(i+1) {
				t.Fatalf("record %d: got %+v, want name %s", i+1, a, name)
			}
			selected, err := list.byCLIName(strings.ToUpper(name))
			if err != nil || selected != a.ID {
				t.Fatalf("lookup %q: got %s, %v", name, selected, err)
			}
		}
		if _, err := list.byCLIName("sarah"); err == nil {
			t.Fatal("an ambiguous old label or email prefix silently selected an account")
		}
	}
	list, err := s.loadClaudeAccounts()
	if err != nil {
		t.Fatal(err)
	}
	check(list)
	again, err := s.loadClaudeAccounts()
	if err != nil {
		t.Fatal(err)
	}
	check(again)
	observed, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(observed, data) {
		t.Fatal("loading the old registry rewrote its contents", err)
	}
	info, err := os.Stat(path)
	if err != nil || !info.ModTime().Equal(stamp) || s.desktopCatalogRevision.Load() != 0 {
		t.Fatal("a read touched the registry or changed its catalog revision", err)
	}
	slices.Reverse(list.Accounts)
	list.Accounts = append(list.Accounts, claudeAccount{ID: id(10), Email: "sarah@another.example", Label: "sarah@another.example"})
	if err = s.saveClaudeAccounts(list); err != nil {
		t.Fatal(err)
	}
	saved, err := s.loadClaudeAccounts()
	if err != nil {
		t.Fatal(err)
	}
	check(saved)
	if got := saved.find(id(10)); got.Name != "account-10" || got.Number != 10 {
		t.Fatalf("a later login reclaimed an ambiguous legacy name: %+v", got)
	}
	if saved.find(id(1)).Label != "Sarah" || saved.find(id(5)).Email != "memo@example.org" {
		t.Fatal("migration changed the original label or identity")
	}
}

func TestClaudeCLINameRenameUsesAccountIDAndPreservesIdentity(t *testing.T) {
	s := testServer(t)
	first := claudeAccount{
		ID: strings.Repeat("a", 24), Number: 7, Name: "Work", Label: "原有备注",
		Email: "qa@example.org", Organization: "team-a", Source: "managed", Status: "ready", ProfileID: strings.Repeat("b", 24),
	}
	other := claudeAccount{ID: strings.Repeat("c", 24), Name: "CAFÉ", Source: "managed", Email: "other@example.org"}
	if err := s.saveClaudeAccounts(claudeAccountList{Accounts: []claudeAccount{first, other}}); err != nil {
		t.Fatal(err)
	}
	update := func(fields string) int {
		t.Helper()
		body := `{"action":"update","id":"` + first.ID + `",` + fields + `}`
		return request(s, "POST", "/api/claude-accounts", s.origin, body, true).Code
	}
	for _, fields := range []string{`"name":""`, `"name":"Start"`, `"name":"001"`, `"name":"cafe\u0301","label":"不可落盘"`, `"name":"bad name"`, `"name":"` + first.ID + `"`} {
		if code := update(fields); code != http.StatusConflict {
			t.Fatalf("invalid rename %s returned %d", fields, code)
		}
	}
	if code := update(`"name":"hijack","email":"changed@example.org"`); code != http.StatusBadRequest {
		t.Fatalf("identity fields should not be accepted through an account update: %d", code)
	}
	if code := update(`"name":"研发.A"`); code != http.StatusOK {
		t.Fatalf("valid account rename: %d", code)
	}
	list, err := s.loadClaudeAccounts()
	if err != nil {
		t.Fatal(err)
	}
	expected := first
	expected.Name = "研发.A"
	if got := list.find(first.ID); got == nil || *got != expected {
		t.Fatalf("renaming the account changed unrelated fields: %+v", got)
	}
	if _, err := list.byCLIName("work"); err == nil {
		t.Fatal("the old command name still resolves after rename")
	}
	id, err := list.byCLIName("研发.a")
	if err != nil || id != first.ID {
		t.Fatal(id, err)
	}
	selected, err := list.explicitCodeAccount(id)
	if err != nil || selected != (claudeSelection{ID: first.ID, GuestID: first.ProfileID, Subject: claudeIdentityHash(first.Email, first.Organization)}) {
		t.Fatal("name changed the selected identity or login directory", selected, err)
	}
	if code := update(`"label":"新备注"`); code != http.StatusOK {
		t.Fatalf("legacy clients can still edit their label: %d", code)
	}
	list, err = s.loadClaudeAccounts()
	if err != nil || list.find(first.ID).Name != expected.Name || list.find(first.ID).Label != "新备注" {
		t.Fatal("a label-only update changed the CLI name", err)
	}
}

func TestClaudeCLINameConflictingSavedSpellingsDoNotSelectAnIdentity(t *testing.T) {
	list := claudeAccountList{Accounts: []claudeAccount{
		{ID: strings.Repeat("a", 24), Name: "Café", Email: "private@example.org"},
		{ID: strings.Repeat("b", 24), Name: "Cafe\u0301", Email: "public@example.org"},
		{ID: strings.Repeat("c", 24), Name: "update", Label: "Tools"},
	}}
	if _, err := list.byCLIName("CAFÉ"); err == nil {
		t.Fatal("duplicate canonical names chose the first account")
	}
	if err := list.prepareCLIAccounts(); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"private", "public", "Tools"} {
		if list.Accounts[i].Name != want {
			t.Fatalf("got %q, want %q", list.Accounts[i].Name, want)
		}
	}
	if _, err := list.byCLIName("café"); err == nil {
		t.Fatal("an ambiguous saved name survived migration")
	}
}

func TestClaudeCLINameNewLoginUsesVerifiedEmailOnlyOnce(t *testing.T) {
	s := testServer(t)
	guest := strings.Repeat("d", 24)
	reading := claudeGuestSnapshot{LoggedIn: true, Email: "member@example.org", Organization: "team-a"}
	list := claudeAccountList{PendingLogin: guest}
	first, err := s.archiveCodeReading(context.Background(), &list, guest, reading)
	if err != nil {
		t.Fatal(err)
	}
	list, err = s.loadClaudeAccounts()
	if err != nil {
		t.Fatal(err)
	}
	a := list.find(first.ID)
	if a == nil || a.Name != "member" || a.Label != reading.Email || first.GuestID != guest || first.Subject != reading.subject() {
		t.Fatalf("unexpected registration: %+v %+v", first, a)
	}
	if err := list.renameCLIAccount(first.ID, "My-Account"); err != nil {
		t.Fatal(err)
	}
	second, err := s.archiveCodeReading(context.Background(), &list, guest, reading)
	if err != nil || first != second {
		t.Fatal("a second verified login changed the binding", second, err)
	}
	list, err = s.loadClaudeAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if got := list.find(first.ID); got == nil || got.Name != "My-Account" || got.Label != reading.Email {
		t.Fatalf("the email suggestion overwrote the selected name: %+v", got)
	}
}

func TestClaudeCLINameSurvivesCodeAndDesktopReauthentication(t *testing.T) {
	s := testServer(t)
	id, guest := strings.Repeat("a", 24), strings.Repeat("b", 24)
	reading := claudeGuestSnapshot{LoggedIn: true, Email: "member@example.org", Organization: "team-x"}
	list := claudeAccountList{Accounts: []claudeAccount{{
		ID: id, Name: "研究-账号", Number: 8, Label: "保留备注", Source: "managed",
		Email: reading.Email, Organization: reading.Organization, CodeUnlinked: true, DesktopInstanceID: "default", DesktopStatus: "ready",
	}}}
	selected, err := s.archiveCodeReading(context.Background(), &list, guest, reading)
	if err != nil || selected != (claudeSelection{ID: id, GuestID: guest, Subject: reading.subject()}) {
		t.Fatalf("linking Code to Desktop: %+v %v", selected, err)
	}
	list, err = s.loadClaudeAccounts()
	if err != nil {
		t.Fatal(err)
	}
	const duplicate = "cccccccccccccccccccccccc"
	list.PendingDesktop = duplicate
	changed, target, restart := list.bindDesktop(duplicate, reading)
	if !changed || !restart || target != "default" || len(list.Accounts) != 1 {
		t.Fatalf("failed to merge the duplicate Desktop: %+v", list)
	}
	if err := s.saveClaudeAccounts(list); err != nil {
		t.Fatal(err)
	}
	list, err = s.loadClaudeAccounts()
	a := list.find(id)
	if err != nil || a == nil || a.Name != "研究-账号" || a.Label != "保留备注" || a.Number != 8 || a.guestID() != guest || a.CodeUnlinked {
		t.Fatalf("Code or Desktop reconciliation lost the account identity/name: %+v %v", a, err)
	}
	for _, update := range []func(*claudeAccount){
		func(a *claudeAccount) { a.CodeUnlinked = true }, func(a *claudeAccount) { a.Status = "changed" },
		func(a *claudeAccount) { a.Email = "invalid-email" }, func(a *claudeAccount) { a.ProfileID = "../other" },
	} {
		copy := *a
		update(&copy)
		if _, err := (claudeAccountList{Accounts: []claudeAccount{copy}}).explicitCodeAccount(id); !errors.Is(err, errClaudeNeedsLogin) {
			t.Fatalf("accepted a nonlaunchable account: %+v %v", copy, err)
		}
	}
}
