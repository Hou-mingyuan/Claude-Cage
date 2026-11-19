package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

type fakeGitHub func(*http.Request) (*http.Response, error)

func (f fakeGitHub) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestIssueRetryAfterLostResponse(t *testing.T) {
	t.Chdir(t.TempDir())
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	config, _ := json.Marshal(appKey{1, string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))})
	if err = os.WriteFile("github-app.json", config, 0600); err != nil {
		t.Fatal(err)
	}
	desk := &issueDesk{dir: "tickets"}
	ticket, err := desk.draft()
	if err != nil {
		t.Fatal(err)
	}
	var posted string
	creates := 0
	old := http.DefaultClient
	defer func() { http.DefaultClient = old }()
	http.DefaultClient = &http.Client{Transport: fakeGitHub(func(r *http.Request) (*http.Response, error) {
		var result string
		switch {
		case r.URL.Host != "api.github.com":
			t.Fatalf("unexpected host %s", r.URL.Host)
		case r.URL.Path == issueRepo+"/installation":
			result = `{"id":7}`
		case r.URL.Path == "/app/installations/7/access_tokens":
			var payload struct {
				Repositories []string
				Permissions  map[string]string
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || len(payload.Repositories) != 1 || payload.Repositories[0] != "Claude-Cage" || len(payload.Permissions) != 1 || payload.Permissions["issues"] != "write" {
				t.Fatal("token is not restricted to project Issues")
			}
			result = `{"token":"test"}`
		case r.URL.Path == issueRepo+"/issues" && r.Method == "POST":
			var data struct{ Body string }
			if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
				t.Fatal(err)
			}
			posted = data.Body
			creates++
			return nil, errors.New("connection ended after write")
		case r.URL.Path == issueRepo+"/issues" && r.Method == "GET":
			b, _ := json.Marshal([]githubIssue{{Number: 7, Body: posted, URL: issueRepoURL + "7"}})
			result = string(b)
		default:
			t.Fatal(r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(result)), Header: make(http.Header)}, nil
	})}
	for i, want := range []int{502, 200, 200} {
		req := httptest.NewRequest("POST", "/ask", nil)
		rec := httptest.NewRecorder()
		desk.publish(rec, req, request{Ticket: ticket, Title: "一个问题", Body: "已确认的说明"}, "127.0.0.1")
		if rec.Code != want {
			t.Fatal(fmt.Sprint(i, ": ", rec.Body.String()))
		}
	}
	if creates != 1 {
		t.Fatalf("created %d issues", creates)
	}
}
