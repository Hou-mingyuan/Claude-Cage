package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Only the service process opens this store. Replace each record atomically before
// making it visible to retrieval or advancing the Feishu inbox cursor.
type lesson struct {
	ID       string `json:"id"`
	Question string `json:"question"`
	Created  int64  `json:"created"`
	Message  string `json:"message,omitempty"`
	Chat     string `json:"chat,omitempty"`
	Answer   string `json:"answer,omitempty"`
	Ignored  bool   `json:"ignored,omitempty"`
	Reply    string `json:"reply,omitempty"`
	Updated  int64  `json:"updated,omitempty"`
}
type lessons struct {
	sync.RWMutex
	dir   string
	items map[string]lesson
}

func openLessons(dir string) (*lessons, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	s := &lessons{dir: dir, items: map[string]lesson{}}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var l lesson
		if err = json.Unmarshal(raw, &l); err != nil || l.ID+".json" != e.Name() || l.Question == "" {
			return nil, fmt.Errorf("invalid lesson file %s", e.Name())
		}
		s.items[l.ID] = l
	}
	return s, nil
}
func writeAtomic(name string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(name), ".pending-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), name); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(name))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (s *lessons) saveLocked(l lesson) error {
	data, err := json.Marshal(l)
	if err != nil {
		return err
	}
	if err = writeAtomic(filepath.Join(s.dir, l.ID+".json"), data); err != nil {
		return err
	}
	s.items[l.ID] = l
	return nil
}

// Reject summaries that still look like contact details, credentials or private paths.
var privateDetails = regexp.MustCompile(`(?i)(https?://|[[:alnum:]._%+-]+@[[:alnum:].-]+\.[a-z]{2,}|(?:/Users/|/home/|[a-z]:\\)|(?:sk-|ghp_|bearer[[:space:]]+)[[:alnum:]_-]{8,}|[0-9]{7,}|(?:[0-9]{1,3}\.){3}[0-9]{1,3}|[a-z0-9_=-]{28,}|-----BEGIN )`)

func (s *lessons) collect(question string) error {
	question = strings.TrimSpace(question)
	if question == "" || utf8.RuneCountInString(question) > 250 || privateDetails.MatchString(question) {
		return nil
	}
	norm := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, question)
	if norm == "" {
		return nil
	}
	hash := sha256.Sum256([]byte(norm))
	id := hex.EncodeToString(hash[:6])
	s.Lock()
	defer s.Unlock()
	if _, ok := s.items[id]; ok {
		return nil
	}
	return s.saveLocked(lesson{ID: id, Question: question, Created: time.Now().Unix()})
}
func (s *lessons) sources(query string) []source {
	if s == nil {
		return nil
	}
	qt := tokens(query)
	type match struct {
		l     lesson
		score float64
	}
	var found []match
	s.RLock()
	defer s.RUnlock()
	for _, l := range s.items {
		if l.Answer == "" || l.Ignored {
			continue
		}
		terms := tokens(l.Question + " " + l.Answer)
		overlap := 0
		for w := range qt {
			if terms[w] {
				overlap++
			}
		}
		score := float64(overlap) / math.Sqrt(float64(len(terms)+1))
		if strings.Contains(query, l.Question) {
			score += 100
		}
		if score > 0 {
			found = append(found, match{l, score})
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].score == found[j].score {
			return found[i].l.ID < found[j].l.ID
		}
		return found[i].score > found[j].score
	})
	var refs []source
	for _, m := range found[:min(len(found), 4)] {
		refs = append(refs, source{Title: "项目维护者答复", Text: "适用问题：" + m.l.Question + "\n维护者答复：" + m.l.Answer})
	}
	return refs
}
func (s *lessons) next() (queued lesson, chat string, first int64) {
	s.RLock()
	defer s.RUnlock()
	for _, l := range s.items {
		if l.Chat != "" {
			chat = l.Chat
			if first == 0 || l.Created < first {
				first = l.Created
			}
		}
		if !l.Ignored && l.Message == "" && (queued.ID == "" || l.Created < queued.Created || (l.Created == queued.Created && l.ID < queued.ID)) {
			queued = l
		}
	}
	return
}
