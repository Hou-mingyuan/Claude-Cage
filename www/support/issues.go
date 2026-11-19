package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type issueTicket struct {
	Created   int64  `json:"created"`
	Submitted int64  `json:"submitted,omitempty"`
	Visitor   string `json:"visitor,omitempty"`
	Number    int    `json:"number,omitempty"`
}
type issueDesk struct {
	mu          sync.Mutex
	dir         string
	lastCleanup int64
}

var ticketID = regexp.MustCompile(`^[a-f0-9]{40}$`)
var exposedKey = regexp.MustCompile(`(?i)(-----BEGIN [A-Z ]*PRIVATE KEY-----|(?:github_pat_|gh[pousr]_|sk-)[a-zA-Z0-9_-]{16,}|Bearer [a-z0-9_.=-]{16,})`)

func (d *issueDesk) save(id string, t issueTicket) error {
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(d.dir, id+".json"), raw)
}
func (d *issueDesk) draft() (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := os.MkdirAll(d.dir, 0700); err != nil {
		return "", err
	}
	now := time.Now()
	if day := now.Unix() / 86400; d.lastCleanup != day {
		if _, _, err := d.capacity("", now); err != nil {
			return "", err
		}
		d.lastCleanup = day
	}
	id := make([]byte, 20)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	key := hex.EncodeToString(id)
	return key, d.save(key, issueTicket{Created: time.Now().Unix()})
}

// Retain receipts across retries and restarts, including an uncertain upstream response.
func (d *issueDesk) capacity(ip string, now time.Time) (string, bool, error) {
	digest := sha256.Sum256([]byte(now.UTC().Format("2006-01-02") + "|" + ip))
	visitor := hex.EncodeToString(digest[:12])
	files, err := os.ReadDir(d.dir)
	if err != nil {
		return "", false, err
	}
	total, personal := 0, 0
	for _, entry := range files {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		p := filepath.Join(d.dir, entry.Name())
		raw, err := os.ReadFile(p)
		if err != nil {
			return "", false, err
		}
		var t issueTicket
		if err = json.Unmarshal(raw, &t); err != nil {
			return "", false, err
		}
		if t.Created < now.Add(-72*time.Hour).Unix() {
			if err = os.Remove(p); err != nil {
				return "", false, err
			}
			continue
		}
		if t.Submitted != 0 && t.Submitted/86400 == now.Unix()/86400 {
			total++
			if visitor == t.Visitor {
				personal++
			}
		}
	}
	return visitor, total < 25 && personal < 3, nil
}
func (d *issueDesk) publish(w http.ResponseWriter, r *http.Request, in request, ip string) {
	if !ticketID.MatchString(in.Ticket) {
		fail(w, 400, "请先让助手生成 Issue 草稿。")
		return
	}
	title, body := strings.TrimSpace(in.Title), strings.TrimSpace(in.Body)
	if title == "" || body == "" || utf8.RuneCountInString(title) > 160 || utf8.RuneCountInString(body) > 6500 {
		fail(w, 400, "请填写标题和正文，并缩短过长的内容。")
		return
	}
	if exposedKey.MatchString(title + " " + body) {
		fail(w, 400, "草稿中可能含有密钥，请移除后再公开。")
		return
	}
	if !d.mu.TryLock() {
		fail(w, 429, "另一条反馈正在提交，请稍后重试。")
		return
	}
	defer d.mu.Unlock()
	var t issueTicket
	raw, err := os.ReadFile(filepath.Join(d.dir, in.Ticket+".json"))
	if err != nil || json.Unmarshal(raw, &t) != nil {
		fail(w, 410, "草稿已过期，请让助手重新整理。")
		return
	}
	success := func(n int) { respond(w, 200, map[string]any{"number": n, "url": issueRepoURL + fmt.Sprint(n)}) }
	if t.Number > 0 {
		success(t.Number)
		return
	}
	now := time.Now()
	if t.Submitted == 0 && t.Created < now.Add(-24*time.Hour).Unix() {
		fail(w, 410, "草稿已过期，请重新生成。")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 24*time.Second)
	defer cancel()
	token, err := botToken(ctx)
	if err != nil {
		logIssue(err)
		fail(w, 503, "项目机器人暂时无法连接 GitHub，草稿仍在这里。")
		return
	}
	marker := "<!-- support-submission:" + in.Ticket + " -->"
	if t.Submitted != 0 {
		since := time.Unix(t.Submitted-90, 0).UTC().Format(time.RFC3339)
		for page := 1; page <= 3; page++ {
			var list []githubIssue
			if err = ghJSON(ctx, "GET", issueRepo+"/issues?state=all&sort=created&direction=desc&per_page=100&page="+fmt.Sprint(page)+"&since="+url.QueryEscape(since), token, nil, &list); err != nil {
				break
			}
			for _, i := range list {
				if i.valid() && strings.Contains(i.Body, marker) {
					t.Number = i.Number
					if err = d.save(in.Ticket, t); err != nil {
						break
					}
					success(i.Number)
					return
				}
			}
			if len(list) < 100 {
				break
			}
		}
		fail(w, 503, "正在核对上一次提交，请稍后重试以取得链接。")
		return
	}
	visitor, ok, err := d.capacity(ip, now)
	if err != nil {
		fail(w, 503, "暂时无法提交，请稍后再试。")
		return
	}
	if !ok {
		fail(w, 429, "当天的官网代发额度已用完，请稍后再试。")
		return
	}
	t.Submitted = now.Unix()
	t.Visitor = visitor
	if err = d.save(in.Ticket, t); err != nil {
		fail(w, 503, "暂时无法保存提交记录。")
		return
	}
	var result githubIssue
	err = ghJSON(ctx, "POST", issueRepo+"/issues", token, map[string]string{"title": title, "body": body + "\n\n" + marker}, &result)
	if err != nil || !result.valid() {
		var status githubStatus
		if errors.As(err, &status) && status >= 400 && status < 500 {
			t.Submitted = 0
			t.Visitor = ""
			_ = d.save(in.Ticket, t)
		}
		logIssue(err)
		fail(w, 502, "GitHub 暂未确认提交，点重试可核对结果。")
		return
	}
	t.Number = result.Number
	if err = d.save(in.Ticket, t); err != nil {
		fail(w, 503, "已提交，正在保存链接，请重试核对。")
		return
	}
	success(t.Number)
}
func logIssue(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "issue bot: %v\n", err)
	}
}
