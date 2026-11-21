package workbench

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
)

func agentTestController(t *testing.T) *Server {
	t.Helper()
	s := testServer(t)
	identity, err := currentController()
	if err != nil {
		t.Fatal(err)
	}
	s.identity = identity
	server := httptest.NewServer(s.handler())
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())
	s.origin = "http://localhost:" + u.Port()
	lock, err := openControllerLock(s.profile, os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	if err = lockController(lock); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unlockController(lock); lock.Close() })
	if err := writePrivate(filepath.Join(s.profile, "control.json"), controlFile{Port: port, Token: s.token, PID: os.Getpid(), Controller: identity}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAgentAuthenticationAndLockedAccount(t *testing.T) {
	s := agentTestController(t)
	if _, err := AgentCall(context.Background(), s.configPath, "settings.get", nil); err != nil {
		t.Fatal(err)
	}
	s.account.lock()
	_, err := AgentCall(context.Background(), s.configPath, "settings.get", nil)
	var ae *AgentError
	if !errors.As(err, &ae) || ae.Code != "account_locked" {
		t.Fatalf("locked: %v", err)
	}
	if _, err := AgentCall(context.Background(), s.configPath, "account.status", nil); err != nil {
		t.Fatal(err)
	}
	if w := request(s, "POST", "/api/agent/settings", s.origin, `{"patch":{"keyboard":"native"}}`, false); w.Code != 401 {
		t.Fatal("agent routes bypassed session", w.Code)
	}
}

// A profile without an account asks for account.create, not account.unlock.
func TestAgentMissingAccountAsksForCreate(t *testing.T) {
	s := agentTestController(t)
	s.account.lock()
	s.account.mu.Lock()
	s.account.record = nil
	s.account.mu.Unlock()
	_, err := AgentCall(context.Background(), s.configPath, "settings.get", nil)
	var ae *AgentError
	if !errors.As(err, &ae) || ae.Code != "account_missing" || ae.NextAction != "account.create" || ae.HTTPStatus != 423 {
		t.Fatalf("missing account: %#v", err)
	}
	if w := request(s, "GET", "/api/settings", "", "", true); w.Code != 423 || w.Header().Get("X-Cage-Recovery") != accountMissingRecovery {
		t.Fatalf("missing account response: %d %q", w.Code, w.Header().Get("X-Cage-Recovery"))
	}
}

func TestAgentSettingsPatchPreservesOmittedFields(t *testing.T) {
	s := agentTestController(t)
	s.preferences.ClaudeTerminalCommand = "claude --verbose"
	before, err := cage.Load(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	data, err := AgentCall(context.Background(), s.configPath, "settings.patch", json.RawMessage(`{"patch":{"browser_home_url":"https://example.org/"},"dry_run":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "https://example.org/") || s.preferences.BrowserHomeURL != "" {
		t.Fatalf("dry run: %s", data)
	}
	data, err = AgentCall(context.Background(), s.configPath, "settings.patch", json.RawMessage(`{"patch":{"browser_home_url":"https://example.org/"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "https://example.org/") {
		t.Fatal("homepage was not confirmed", string(data))
	}
	if s.preferences.ClaudeTerminalCommand != "claude --verbose" || s.preferences.BrowserHomeURL != "https://example.org/" {
		t.Fatalf("preferences lost: %+v", s.preferences)
	}
	after, _ := cage.Load(s.configPath)
	if before != after {
		t.Fatal("unrelated network config changed")
	}
	for _, input := range []string{`{"patch":{"proxy_url":"socks5://127.0.0.1:1"}}`, `{"patch":{"keyboard":null}}`, `{"patch":{"keyboard":"invalid"}}`, `{"patch":{}}`} {
		if _, err := AgentCall(context.Background(), s.configPath, "settings.patch", json.RawMessage(input)); err == nil {
			t.Fatal("accepted invalid patch", input)
		}
	}
}

func TestAgentPartialExitPolicyPreservesAnchor(t *testing.T) {
	s := agentTestController(t)
	cfg, err := cage.Load(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ExitPolicy = cage.ExitPolicy{Mode: "dynamic", Scope: "city", IP: "8.8.8.8", CountryCode: "US", Region: "California", City: "Los Angeles"}
	if err := writePrivate(s.configPath, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := AgentCall(context.Background(), s.configPath, "settings.patch", json.RawMessage(`{"patch":{"exit_policy":{"mode":"static"}},"dry_run":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var view Settings
	_ = json.Unmarshal(data, &view)
	if view.ExitPolicy.Mode != "static" || view.ExitPolicy.Scope != "city" || view.ExitPolicy.IP != "8.8.8.8" || view.ExitPolicy.City != "Los Angeles" {
		t.Fatalf("network anchor discarded: %+v", view.ExitPolicy)
	}
	_, err = AgentCall(context.Background(), s.configPath, "settings.patch", json.RawMessage(`{"patch":{"exit_policy":{"ip":null}},"dry_run":true}`))
	if err == nil {
		t.Fatal("null cleared network anchor")
	}
}

func TestAgentJobIdentityAndCancellation(t *testing.T) {
	s := agentTestController(t)
	s.setJobForTest(Job{ID: "012345abcdef", Running: true})
	if _, err := AgentJob(context.Background(), s.configPath, "012345abcdef"); err != nil {
		t.Fatal(err)
	}
	s.setJobForTest(Job{ID: "abcdef012345"})
	_, err := AgentJob(context.Background(), s.configPath, "012345abcdef")
	var ae *AgentError
	if !errors.As(err, &ae) || ae.Code != "job_not_found" {
		t.Fatalf("wrong job accepted: %v", err)
	}
	var manager jobManager
	manager.state = Job{ID: "012345abcdef", Running: true}
	execution := &jobExecution{id: manager.state.ID, ctx: context.Background()}
	manager.finishLocked(execution, context.Canceled)
	if manager.state.Running || !manager.state.Canceled {
		t.Fatalf("cancellation hidden: %+v", manager.state)
	}
}

func TestAgentRedactionPreservesOperationalValues(t *testing.T) {
	input := `{"id":"012345abcdef","clone_key":"` + strings.Repeat("a", 64) + `","workspace":"/Users/test/project","dns_url":"https://1.1.1.1/dns-query","error":"fetch https://secret.example/sub?token=very-secret failed","job":{"log":"password=hidden"}}`
	output := agentRedact(input)
	if !json.Valid([]byte(output)) || strings.Contains(output, "very-secret") || strings.Contains(output, "hidden") {
		t.Fatal(output)
	}
	for _, value := range []string{"012345abcdef", strings.Repeat("a", 64), "/Users/test/project", "https://1.1.1.1/dns-query"} {
		if !strings.Contains(output, value) {
			t.Fatal("operational field redacted", output)
		}
	}
}

func TestAgentNoImplicitStartAndUnknownInput(t *testing.T) {
	profile := t.TempDir()
	path := filepath.Join(profile, "config.json")
	for _, tc := range []struct{ op, input, code string }{{"does.not.exist", "{}", "unknown_operation"}, {"environment.stop", `{"surprise":true}`, "invalid_input"}, {"settings.get", "{}", "service_unavailable"}} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := AgentCall(ctx, path, tc.op, json.RawMessage(tc.input))
		cancel()
		var ae *AgentError
		if !errors.As(err, &ae) || ae.Code != tc.code {
			t.Fatalf("%s: %v", tc.op, err)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("read created profile")
	}
}

func TestAgentCannotFollowSelectedRemote(t *testing.T) {
	s := agentTestController(t)
	s.remote = newRemoteGateway(s.profile)
	t.Cleanup(func() { s.remote.close() })
	s.remote.setRemote(true)
	_, err := AgentCall(context.Background(), s.configPath, "environment.stop", json.RawMessage(`{}`))
	var ae *AgentError
	if !errors.As(err, &ae) || ae.Code != "state_conflict" {
		t.Fatalf("remote target accepted: %v", err)
	}
	if s.jobs.snapshot().ID != "" {
		t.Fatal("remote rejection fell back to local mutation")
	}
}

func TestAgentRejectsOtherConfigFileInSameProfile(t *testing.T) {
	s := agentTestController(t)
	_, err := AgentCall(context.Background(), filepath.Join(s.profile, "other.json"), "account.status", nil)
	var ae *AgentError
	if !errors.As(err, &ae) || ae.Code != "config_path_mismatch" {
		t.Fatalf("wrong config accepted: %v", err)
	}
	s.clientOnly = true
	_, err = AgentCall(context.Background(), s.configPath, "account.status", nil)
	if !errors.As(err, &ae) || ae.Code != "controller_mismatch" {
		t.Fatalf("remote-only client accepted: %v", err)
	}
}

func TestAgentNativeDiagnosticExport(t *testing.T) {
	for _, system := range []string{"linux"} {
		t.Run(system, func(t *testing.T) {
			s := agentTestController(t)
			s.account.lock()
			path := filepath.Join(t.TempDir(), "diagnostics.zip")
			if err := ExportAgentDiagnostics(s.configPath, path); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil || len(before) == 0 {
				t.Fatal("missing bundle", err)
			}
			archive, err := zip.OpenReader(path)
			if err != nil {
				t.Fatal(err)
			}
			defer archive.Close()
			for _, file := range archive.File {
				if file.Name == "manifest.json" {
					reader, err := file.Open()
					if err != nil {
						t.Fatal(err)
					}
					body, _ := io.ReadAll(reader)
					reader.Close()
					if !strings.Contains(string(body), `"guest_collection": "environment unavailable"`) {
						t.Fatal("did not use authenticated native diagnostic endpoint", string(body))
					}
				}
			}
			if err := ExportAgentDiagnostics(s.configPath, path); err == nil {
				t.Fatal("overwrote previous diagnostic")
			}
			after, _ := os.ReadFile(path)
			if string(before) != string(after) {
				t.Fatal("existing bundle changed")
			}
		})
	}
}

func TestAgentTrustedLoginOmitsRejectedCreate(t *testing.T) {
	for _, op := range AgentOperations() {
		if op.Name != "client.trusted-login" {
			continue
		}
		if strings.Contains(strings.ReplaceAll(op.Description, "account.create", ""), "create") || !strings.Contains(op.Description, "account.create") {
			t.Fatalf("trusted-login advertises an action the launcher rejects: %q", op.Description)
		}
		return
	}
	t.Fatal("client.trusted-login is not listed")
}

func TestAgentStartRejectedDuringDesktopOnboardingAndAllowedHeadless(t *testing.T) {
	s := agentTestController(t)
	// Headless agent calling initialization.set begin does not claim desktop onboarding.
	if _, err := AgentCall(context.Background(), s.configPath, "initialization.set", json.RawMessage(`{"action":"begin"}`)); err != nil {
		t.Fatalf("agent begin: %v", err)
	}
	raw, err := AgentCall(context.Background(), s.configPath, "initialization.status", nil)
	if err != nil {
		t.Fatalf("initialization.status: %v", err)
	}
	var view initializationView
	if err := json.Unmarshal(raw, &view); err != nil || !view.Required || view.DesktopActive {
		t.Fatalf("agent begin unexpectedly activated desktop onboarding: %+v %v", view, err)
	}

	// Desktop onboarding screen polling /api/status with setup initiator claims the lease.
	w := requestWithHeader(s, "GET", "/api/status", "", jobInitiatorHeader, jobInitiatorSetup)
	if w.Code != 200 {
		t.Fatalf("setup status: %d %s", w.Code, w.Body.String())
	}
	raw, err = AgentCall(context.Background(), s.configPath, "initialization.status", nil)
	if err != nil {
		t.Fatalf("initialization.status after setup poll: %v", err)
	}
	if err := json.Unmarshal(raw, &view); err != nil || !view.DesktopActive {
		t.Fatalf("expected desktop_active=true: %+v %v", view, err)
	}

	// Agent environment.start must be rejected with state_conflict without starting a job.
	_, err = AgentCall(context.Background(), s.configPath, "environment.start", json.RawMessage(`{}`))
	var ae *AgentError
	if !errors.As(err, &ae) || ae.Code != "state_conflict" || ae.HTTPStatus != 409 || !strings.Contains(ae.Message, "首次引导") {
		t.Fatalf("expected state_conflict during desktop onboarding, got: %v", err)
	}
	if s.jobs.snapshot().ID != "" {
		t.Fatalf("rejected agent start published a job: %+v", s.jobs.snapshot())
	}

	// Once onboarding lease expires, agent start is no longer blocked by desktop onboarding.
	s.clearDesktopOnboarding()
	_, err = AgentCall(context.Background(), s.configPath, "environment.start", json.RawMessage(`{"guest_system":"macos"}`))
	if !errors.As(err, &ae) || strings.Contains(ae.Message, "首次引导") {
		t.Fatalf("stale onboarding guard remained active: %v", err)
	}
}

func TestAgentJobExposesInitiatorAndCanceledByWhenStoppedByDesktop(t *testing.T) {
	s := agentTestController(t)
	started, canceled := make(chan struct{}), make(chan struct{})
	first, err := s.submitJobWithInitiator(jobInitiatorAgent, "start", func() error {
		close(started)
		<-s.operationContext().Done()
		close(canceled)
		return context.Canceled
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitJobSignal(t, started)

	// Desktop setup screen issues POST /api/stop.
	w := requestWithHeader(s, "POST", "/api/stop", "{}", jobInitiatorHeader, jobInitiatorSetup)
	if w.Code != 200 {
		t.Fatalf("setup stop failed: %d %s", w.Code, w.Body.String())
	}
	var stopJob Job
	if err := json.Unmarshal(w.Body.Bytes(), &stopJob); err != nil || stopJob.Initiator != jobInitiatorSetup {
		t.Fatalf("stop job missing setup initiator: %+v %v", stopJob, err)
	}
	awaitJobSignal(t, canceled)

	data, err := AgentJob(context.Background(), s.configPath, first.ID)
	if err != nil {
		t.Fatalf("AgentJob(%s): %v", first.ID, err)
	}
	var queried Job
	if err := json.Unmarshal(data, &queried); err != nil {
		t.Fatal(err)
	}
	if queried.Initiator != jobInitiatorAgent || !queried.Canceled || queried.CanceledBy != jobInitiatorSetup || queried.SupersededBy != stopJob.ID {
		t.Fatalf("agent job missing cancellation source: %+v", queried)
	}
}

func requestWithHeader(s *Server, method, path, body, header, value string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, s.origin+path, strings.NewReader(body))
	r.Header.Set("Origin", s.origin)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set(header, value)
	r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
	r.AddCookie(&http.Cookie{Name: s.cookieName() + "-desktop", Value: trustedViewProof(s.token)})
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	return w
}

func TestAgentReleasesDesktopOnboardingWithoutQuittingWindow(t *testing.T) {
	s := agentTestController(t)
	desktopActive := func() bool {
		t.Helper()
		raw, err := AgentCall(context.Background(), s.configPath, "initialization.status", nil)
		var view initializationView
		if err != nil || json.Unmarshal(raw, &view) != nil {
			t.Fatalf("initialization.status: %s %v", raw, err)
		}
		return view.DesktopActive
	}
	poll := func() {
		t.Helper()
		for _, path := range []string{"/api/status", "/api/environment/initialization"} {
			if w := requestWithHeader(s, "GET", path, "", jobInitiatorHeader, jobInitiatorSetup); w.Code != 200 {
				t.Fatalf("setup poll %s: %d %s", path, w.Code, w.Body.String())
			}
		}
	}
	poll()
	if !desktopActive() {
		t.Fatal("onboarding poll did not claim the lease")
	}
	if _, err := AgentCall(context.Background(), s.configPath, "initialization.set", json.RawMessage(`{"action":"release"}`)); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.profile, "initialization.json")); err != nil {
		t.Fatal("release did not record the started setup, so a reopened window would claim it again", err)
	}
	// The still-open onboarding screen keeps polling.
	poll()
	if desktopActive() {
		t.Fatal("onboarding polling reclaimed a released lease")
	}
	var ae *AgentError
	if _, err := AgentCall(context.Background(), s.configPath, "environment.start", json.RawMessage(`{"guest_system":"macos"}`)); !errors.As(err, &ae) || strings.Contains(ae.Message, "首次引导") {
		t.Fatalf("released onboarding still blocked the agent: %v", err)
	}
	// Starting the install from the window takes setup back.
	if w := initializationRequest(s, "install", true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if !desktopActive() {
		t.Fatal("a desktop install did not reclaim onboarding")
	}
	if _, err := AgentCall(context.Background(), s.configPath, "environment.start", json.RawMessage(`{}`)); !errors.As(err, &ae) || ae.Code != "state_conflict" {
		t.Fatalf("agent start during reclaimed onboarding: %v", err)
	}
}
