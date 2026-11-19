package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const issueRepo = "/repos/Lee-xeo/Claude-Cage"
const issueRepoURL = "https://github.com/Lee-xeo/Claude-Cage/issues/"

type appKey struct {
	ID  int64  `json:"id"`
	PEM string `json:"pem"`
}
type githubStatus int

func (s githubStatus) Error() string { return fmt.Sprintf("GitHub status %d", int(s)) }
func ghJSON(ctx context.Context, method, path, token string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.github.com"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "website-support-assistant")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return githubStatus(resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}
func (key appKey) jwt(now time.Time) (string, error) {
	block, _ := pem.Decode([]byte(key.PEM))
	if block == nil || key.ID < 1 {
		return "", errors.New("invalid app credential")
	}
	secret, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding.EncodeToString
	claims, _ := json.Marshal(map[string]any{"iss": strconv.FormatInt(key.ID, 10), "iat": now.Unix() - 60, "exp": now.Unix() + 480})
	input := enc([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc(claims)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, secret, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return input + "." + enc(sig), nil
}
func botToken(ctx context.Context) (string, error) {
	raw, err := os.ReadFile("github-app.json")
	if err != nil {
		return "", err
	}
	var key appKey
	if err = json.Unmarshal(raw, &key); err != nil {
		return "", err
	}
	jwt, err := key.jwt(time.Now())
	if err != nil {
		return "", err
	}
	var install struct{ ID int64 }
	if err = ghJSON(ctx, "GET", issueRepo+"/installation", jwt, nil, &install); err != nil {
		return "", err
	}
	var grant struct{ Token string }
	path := fmt.Sprintf("/app/installations/%d/access_tokens", install.ID)
	err = ghJSON(ctx, "POST", path, jwt, map[string]any{"repositories": []string{"Claude-Cage"}, "permissions": map[string]string{"issues": "write"}}, &grant)
	if err != nil {
		return "", err
	}
	if grant.Token == "" {
		return "", errors.New("missing installation token")
	}
	return grant.Token, nil
}

type githubIssue struct {
	Number int
	Body   string
	URL    string `json:"html_url"`
}

func (i githubIssue) valid() bool {
	return i.Number > 0 && strings.EqualFold(i.URL, issueRepoURL+strconv.Itoa(i.Number))
}
