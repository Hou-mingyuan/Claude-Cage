package workbench

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// The guest account helper records configuration problems it met while
// preparing an account for Desktop or a terminal. Reading them is a small local
// file read in the guest; it never starts the CLI or contacts Anthropic.
type claudeAccountNotice struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Item    string `json:"item,omitempty"`
	Local   string `json:"local,omitempty"`
	Shared  string `json:"shared,omitempty"`
	Since   int64  `json:"since,omitempty"`
	Message string `json:"message,omitempty"`
}

const (
	claudeNoticesFresh   = 20 * time.Second
	claudeNoticesTimeout = 4 * time.Second
	claudeNoticesLimit   = 32
)

// readClaudeGuestNotices mirrors readClaudeGuestQuotas. Completed chunks are
// kept when a later one fails; an older guest without the mode returns an error.
func (s *Server) readClaudeGuestNotices(ctx context.Context, ids []string) (map[string][]claudeAccountNotice, error) {
	notices := make(map[string][]claudeAccountNotice)
	for chunk := range slices.Chunk(ids, 24) {
		data, err := s.guest(ctx, "exec /usr/local/bin/cage-claude-account notices "+strings.Join(chunk, " "))
		if err != nil {
			return notices, err
		}
		var reading map[string][]claudeAccountNotice
		if err := json.Unmarshal(data, &reading); err != nil {
			return notices, err
		}
		for _, id := range chunk {
			if list := cleanClaudeNotices(reading[id]); len(list) > 0 {
				notices[id] = list
			}
		}
	}
	return notices, nil
}

func cleanClaudeNotices(in []claudeAccountNotice) []claudeAccountNotice {
	var out []claudeAccountNotice
	for _, n := range in {
		if n.ID == "" || (n.Kind == "" && n.Message == "") {
			continue
		}
		n.ID, n.Kind, n.Item = clipNoticeText(n.ID, 64), clipNoticeText(n.Kind, 64), clipNoticeText(n.Item, 512)
		n.Local, n.Shared, n.Message = clipNoticeText(n.Local, 1024), clipNoticeText(n.Shared, 1024), clipNoticeText(n.Message, 2048)
		out = append(out, n)
		if len(out) == claudeNoticesLimit {
			break
		}
	}
	return out
}

func clipNoticeText(value string, limit int) string {
	value = strings.ToValidUTF8(value, "")
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

// claudeAccountNotices returns the cached notices, refreshing them once they
// are stale. Any failure means "no notices"; it never fails the account list.
func (s *Server) claudeAccountNotices(parent context.Context, accounts []claudeAccount) map[string][]claudeAccountNotice {
	var ids []string
	for _, a := range accounts {
		if id := a.guestID(); validClaudeGuestID(id) && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	key := strings.Join(ids, " ")
	m := &s.claudeAccounts
	m.mu.Lock()
	cached := m.notices
	if m.noticesFetching || (m.noticesKey == key && time.Since(m.noticesAt) < claudeNoticesFresh) {
		m.mu.Unlock()
		return cached
	}
	m.noticesFetching = true
	read := m.noticeProbe
	m.mu.Unlock()
	if read == nil {
		read = s.readClaudeGuestNotices
	}
	notices := map[string][]claudeAccountNotice{}
	if epoch, unlocked := s.account.access(); unlocked {
		ctx, cancel := context.WithTimeout(parent, claudeNoticesTimeout)
		stop := context.AfterFunc(epoch, cancel)
		reading, err := read(ctx, ids)
		stop()
		cancel()
		if err == nil || len(reading) > 0 {
			notices = reading
		}
	}
	m.mu.Lock()
	m.notices, m.noticesKey, m.noticesAt, m.noticesFetching = notices, key, time.Now(), false
	m.mu.Unlock()
	return notices
}
