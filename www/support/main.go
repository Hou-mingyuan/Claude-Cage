package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

//go:embed prompt.md
var prompt string

const origin = "https://claude-cage.com"

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type request struct {
	Proof        string    `json:"proof"`
	LogExcerpt   string    `json:"logExcerpt,omitempty"`
	SuggestTitle bool      `json:"suggestTitle"`
	Action       string    `json:"action"`
	Ticket       string    `json:"ticket"`
	Title        string    `json:"title"`
	Body         string    `json:"body"`
	Question     string    `json:"question"`
	History      []message `json:"history"`
	Web          bool      `json:"web"`
	Photo        string    `json:"photo,omitempty"`
	Collect      bool      `json:"collect"`
}
type quota struct {
	sync.Mutex
	hour, day int64
	total     int
	ips       map[string]int
}

func (q *quota) take(ip string, now time.Time) bool {
	q.Lock()
	defer q.Unlock()
	h := now.Unix() / 3600
	d := h / 24
	if h != q.hour {
		q.hour = h
		q.ips = map[string]int{}
	}
	if d != q.day {
		q.day = d
		q.total = 0
	}
	if q.total >= 600 || q.ips[ip] >= 20 {
		return false
	}
	q.total++
	q.ips[ip]++
	return true
}
func respond(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
func fail(w http.ResponseWriter, status int, msg string) {
	respond(w, status, map[string]string{"error": msg})
}

type issueDraft struct {
	Ticket string `json:"ticket,omitempty"`
	Title  string `json:"title"`
	Body   string `json:"body"`
}
type decision struct {
	ChatTitle string      `json:"chatTitle,omitempty"`
	Issue     *issueDraft `json:"issue,omitempty"`
	Answer    string      `json:"answer"`
	Status    string      `json:"status"`
	Policy    string      `json:"policy"`
	Lookups   []string    `json:"lookups"`
	Question  string      `json:"question"`
}

func modelJSON(ctx context.Context, key string, messages []message, output any) error {
	raw, err := json.Marshal(messages)
	if err != nil {
		return err
	}
	text, err := modelText(ctx, key, raw, true)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(text), output)
}
func modelText(ctx context.Context, key string, messages json.RawMessage, jsonMode bool) (string, error) {
	payload := map[string]any{"model": "deepseek-flash", "messages": messages, "max_tokens": 1800, "stream": false, "thinking": map[string]string{"type": "disabled"}}
	if jsonMode {
		payload["response_format"] = map[string]string{"type": "json_object"}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.token-link.shop/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("model status %d", resp.StatusCode)
	}
	var result struct{ Choices []struct{ Message message } }
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	if len(result.Choices) == 0 || strings.TrimSpace(result.Choices[0].Message.Content) == "" {
		return "", errors.New("empty model response")
	}
	return result.Choices[0].Message.Content, nil
}
func complete(ctx context.Context, key string, messages []message) (decision, error) {
	var reply decision
	if err := modelJSON(ctx, key, messages, &reply); err != nil {
		return decision{}, err
	}
	if strings.TrimSpace(reply.Answer) == "" {
		return decision{}, errors.New("invalid answer JSON")
	}
	return reply, nil
}
func main() {
	key := os.Getenv("TOKEN_LINK_API_KEY")
	if key == "" {
		log.Fatal("missing model credential")
	}
	rev, err := os.ReadFile("corpus.tar.gz.rev")
	if err != nil {
		log.Fatal(err)
	}
	sha := strings.TrimSpace(string(rev))
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(sha) {
		log.Fatal("invalid corpus revision")
	}
	ix, err := loadIndex("corpus.tar.gz", sha)
	if err != nil {
		log.Fatal(err)
	}
	var learned *lessons
	var issues *issueDesk
	if dir := os.Getenv("SUPPORT_LEARNING_DIR"); dir != "" {
		issues = &issueDesk{dir: filepath.Join(filepath.Dir(dir), "issue-drafts")}
		learned, err = openLessons(dir)
		if err != nil {
			log.Fatal(err)
		}
		if owner := os.Getenv("SUPPORT_FEISHU_OWNER"); owner != "" {
			cli := os.Getenv("SUPPORT_FEISHU_CLI")
			if cli == "" {
				cli = "lark-cli"
			}
			go (feishu{store: learned, owner: owner, cli: cli, confirms: directAnswerCheck(key)}).run()
		}
	}
	mux := http.NewServeMux()
	lim := &quota{}
	slots := make(chan struct{}, 5)
	mux.HandleFunc("/ask", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Vary", "Origin")
		if r.Header.Get("Origin") != origin {
			fail(w, 403, "请从官网答疑页提问。")
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		if r.Method == "OPTIONS" {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.WriteHeader(204)
			return
		}
		if r.Method == "GET" {
			protectionConfig(w, r)
			return
		}
		if r.Method != "POST" {
			fail(w, 405, "请使用提问表单。")
			return
		}
		// The reverse proxy overwrites this header with the actual peer address.
		ip := net.ParseIP(r.Header.Get("X-Real-IP"))
		if ip == nil {
			host, _, _ := net.SplitHostPort(r.RemoteAddr)
			ip = net.ParseIP(host)
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			fail(w, 429, "当前提问较多，请稍后再试。")
			return
		}

		var in request
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2200<<10))
		if d.Decode(&in) != nil || d.Decode(&struct{}{}) != io.EOF || len(in.History) > 12 || (in.Photo != "" && !validPhoto(in.Photo)) {
			fail(w, 400, "问题过长或格式有误。")
			return
		}
		if in.Action != "" && in.Action != "publish_issue" {
			fail(w, 400, "未知操作。")
			return
		}
		action := "ask"
		if in.Action == "publish_issue" {
			action = "issue"
		}
		verified, verifyErr := validatePageProof(r.Context(), in.Proof, action)
		if verifyErr != nil {
			fail(w, 503, "页面验证暂不可用，请稍后重试。")
			return
		}
		if !verified {
			fail(w, 403, "页面验证无效或已过期，请在官网答疑页重新发送。")
			return
		}
		if in.Action == "publish_issue" {
			if issues == nil {
				fail(w, 503, "机器人暂不可用。")
				return
			}
			issues.publish(w, r, in, ip.String())
			return
		}
		in.Question = strings.TrimSpace(in.Question)
		if utf8.RuneCountInString(in.LogExcerpt) > 12000 {
			fail(w, 400, "日志节选过长，请缩短后重试。")
			return
		}
		in.LogExcerpt = strings.TrimSpace(in.LogExcerpt)
		if in.Question == "" && in.LogExcerpt != "" {
			in.Question = "请分析诊断日志并给出下一步。"
		}
		if in.Question == "" && in.Photo != "" {
			in.Question = "请看看这张截图，帮我解释当前情况。"
		}
		if in.Question == "" || utf8.RuneCountInString(in.Question) > 1400 {
			fail(w, 400, "请输入 1–1400 个字符的问题。")
			return
		}
		query := in.Question
		for i, m := range in.History {
			if (i%2 == 0 && m.Role != "user") || (i%2 == 1 && m.Role != "assistant") || utf8.RuneCountInString(m.Content) > 1800 || len(in.History)%2 != 0 {
				fail(w, 400, "对话格式有误，请重新提问。")
				return
			}
		}
		if len(in.History) >= 2 {
			query += " " + in.History[len(in.History)-2].Content
		}
		if !lim.take(ip.String(), time.Now()) {
			fail(w, 429, "当前提问额度已用完，请稍后再试。")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 75*time.Second)
		defer cancel()
		rules, err := os.ReadFile("rules.md")
		if err != nil {
			fail(w, 503, "答疑规则暂不可用，请稍后再试。")
			return
		}
		observation := ""
		if in.Photo != "" {
			observation, err = inspectPhoto(ctx, key, in.Photo, in.Question)
			if err != nil {
				log.Printf("image observation unavailable: %v", err)
				fail(w, 502, "图片暂时未能识别，请重试或粘贴报错原文。")
				return
			}
			query += " " + observation
		}
		if in.LogExcerpt != "" {
			query += " " + in.LogExcerpt
		}
		plan := planLookup(ctx, key, in.Question)
		refs := append(learned.sources(query), ix.search(query, "")...)
		refs = ix.investigate(refs, plan.Queries)
		external := 0
		if in.Web {
			webCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			ws := webSources(webCtx, in.Question)
			stop()
			external = len(ws)
			refs = append(refs, ws...)
		}
		system := prompt + "\n维护者规则：\n" + string(rules)
		if in.SuggestTitle {
			system += "\n本轮请同时生成历史对话标题：根据访客描述、近期对话和截图中的主题，在 JSON 的 chatTitle 字段给出一个便于日后查找的短标题。优先点明具体功能和问题，用访客的语言，例如「Windows 安装卡在启动环境」。中文通常 6–18 字，不加引号、序号、句号，不抄开场白、账号、路径、密钥或私人信息。仅有寒暄、尚无明确主题时省略 chatTitle。"
		}
		messages := append([]message{{"system", system}}, in.History...)
		messages = append(messages, message{"user", fmt.Sprintf("用户问题：\n%s\n\n截图中可见的信息：\n%s\n\n访客提供的诊断日志节选（不完整）：\n%s\n\n可参考资料（外部搜索命中 %d 条）：\n%s", in.Question, observation, in.LogExcerpt, external, numberedSources(refs, 0))})
		answer, err := complete(ctx, key, messages)
		if err != nil {
			log.Printf("answer unavailable: %v", err)
			respond(w, 200, map[string]any{"answer": referenceOnly(in.Question, refs), "sources": refs, "webCount": external, "observation": observation, "issue": nil})
			return
		}
		reviewed := false
		if answer.Status == "lookup" || answer.Status == "knowledge_gap" {
			oldLen := len(refs)
			refs = ix.investigate(refs, answer.Lookups)
			follow := fmt.Sprintf("核查刚才的初稿。请结合新增检索到的资料和前文编号重新给出完整 JSON。不论技术细节是否查到，都直接回答能确认的部分或问访客要证据；查不到的兼容性要说明尚无实测。只有访客原本就在问尚未确定的商务条款、服务承诺或未来计划时可保留 knowledge_gap 并填 policy。不要将技术疑问改写为对维护者的政策提问。不要再发起 lookup。初稿：%s\n新增资料：\n%s", answer.Answer, numberedSources(refs[oldLen:], oldLen))
			revised, reviewErr := complete(ctx, key, append(messages, message{"user", follow}))
			if reviewErr == nil {
				answer = revised
				reviewed = true
			}
		}
		if learned != nil && in.Collect && reviewed && plan.canAskOwner(answer) {
			if err := learned.collect(answer.Question); err != nil {
				log.Printf("question storage: %v", err)
			}
		}
		if answer.Issue != nil {
			answer.Issue.Ticket = ""
			if issues != nil && answer.Issue.Title != "" && answer.Issue.Body != "" {
				answer.Issue.Ticket, err = issues.draft()
				if err != nil {
					fail(w, 503, "暂时无法保存 Issue 草稿。")
					return
				}
			}
		}
		reply := map[string]any{"answer": answer.Answer, "sources": refs, "webCount": external, "observation": observation, "issue": answer.Issue}
		if title := strings.Join(strings.Fields(answer.ChatTitle), " "); in.SuggestTitle && title != "" && !privateDetails.MatchString(title) {
			runes := []rune(title)
			reply["chatTitle"] = string(runes[:min(len(runes), 48)])
		}
		respond(w, 200, reply)
	})
	server := &http.Server{Addr: "127.0.0.1:18152", Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 82 * time.Second, IdleTimeout: 35 * time.Second}
	log.Fatal(server.ListenAndServe())
}
