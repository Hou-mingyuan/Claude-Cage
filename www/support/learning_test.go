package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestOnlyOwnerReplyBecomesReusableKnowledge(t *testing.T) {
	store, err := openLessons(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	question := "团队用户在哪里预约入门指导"
	for _, q := range []string{question + "？", question + "?", "联系 foo@example.org 可以预约吗？"} {
		if err = store.collect(q); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.items) != 1 {
		t.Fatalf("saved %d questions", len(store.items))
	}
	l, _, _ := store.next()
	l.Chat = "dm"
	l.Message = "question-msg"
	if err = store.saveLocked(l); err != nil {
		t.Fatal(err)
	}
	f := feishu{store: store, owner: "owner"}
	var m larkMessage
	m.Chat = l.Chat
	m.Parent = l.Message
	m.Type = "text"
	m.Sender.Type = "user"
	m.Sender.ID = "stranger"
	set := func(id int, text string) {
		m.ID = fmt.Sprint(id)
		m.Created = fmt.Sprint(id)
		b, _ := json.Marshal(map[string]string{"text": text})
		m.Body.Content = string(b)
		if err = f.apply(m, l.Chat, ""); err != nil {
			t.Fatal(err)
		}
	}
	set(1, "虚假的指引")
	m.Sender.ID = "owner"
	m.Parent = "unrelated"
	set(2, "我的私人消息")
	if len(store.sources(question)) != 0 {
		t.Fatal("unverified message was learned")
	}
	m.Parent = l.Message
	set(3, "通过工作台预约团队指导")
	m.Parent = ""
	set(4, "答 "+l.ID+" 联系项目维护者预约团队指导")
	m.Parent = l.Message
	set(3, "旧答案")
	restored, err := openLessons(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	refs := restored.sources("如何预约团队指导")
	if len(refs) != 1 || !strings.Contains(refs[0].Text, "联系项目维护者") {
		t.Fatalf("latest durable answer missing: %v", refs)
	}
	set(5, "忽略")
	if len(store.sources(question)) != 0 {
		t.Fatal("withdrawn answer was retrieved")
	}
}

func TestBareReplyRequiresOneAdjacentQuestionAndAnAnswer(t *testing.T) {
	store, err := openLessons(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first := lesson{ID: "a", Chat: "dm", Message: "ask-a", Question: "提供辅导吗？"}
	second := lesson{ID: "b", Chat: "dm", Message: "ask-b", Question: "支持预约吗？"}
	store.items[first.ID] = first
	calls := 0
	f := feishu{store: store, owner: "owner", confirms: func(q, reply string) (bool, error) {
		calls++
		return q == first.Question && reply == "暂不提供辅导", nil
	}}
	m := larkMessage{ID: "reply", Chat: "dm", Type: "text", Created: "123"}
	m.Sender.ID = "owner"
	m.Sender.Type = "user"
	run := func(previous, text string) {
		b, _ := json.Marshal(map[string]string{"text": text})
		m.Body.Content = string(b)
		if err := f.apply(m, "dm", previous); err != nil {
			t.Fatal(err)
		}
	}
	run("other", "暂不提供辅导")
	store.items[second.ID] = second
	run("ask-a", "暂不提供辅导")
	if calls != 0 {
		t.Fatal("unlinked private messages reached the classifier")
	}
	delete(store.items, second.ID)
	run("ask-a", "稍后回复")
	if store.items[first.ID].Answer != "" {
		t.Fatal("acknowledgment became public")
	}
	run("ask-a", "暂不提供辅导")
	run("ask-a", "暂不提供辅导")
	if calls != 2 || store.items[first.ID].Answer != "暂不提供辅导" {
		t.Fatal("answer was not bound exactly once")
	}
}
