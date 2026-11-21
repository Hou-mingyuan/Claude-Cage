package workbench

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
)

func setupUnreachableManagedProxy(t *testing.T, s *Server) (cage.Config, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	store, err := managedproxy.Load(s.profile)
	if err != nil {
		t.Fatal(err)
	}
	store.Port = port
	if err := store.Save(s.profile); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ProxyURL = store.Endpoint()
	cfg.ProxyRevision = store.Revision
	cfg.ExitPolicy = cage.ExitPolicy{Mode: "dynamic", Scope: "country", CountryCode: "US"}
	if err := writePrivate(s.configPath, cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.RequireManagedProxy(s.profile); err != nil {
		t.Fatalf("proxy fixture not applied: %v", err)
	}
	return cfg, store.Selected
}

func TestStaticIPDraftSaveAndFailClosedSecurity(t *testing.T) {
	s := agentTestController(t)
	baseCfg, selectedNode := setupUnreachableManagedProxy(t, s)

	t.Run("default save rejects and exposes draft recovery without persisting", func(t *testing.T) {
		next := baseCfg
		next.ExitPolicy = cage.ExitPolicy{Mode: "static", Scope: "country", IP: "8.8.8.8"}
		job, err := s.submitJob("settings", func() error {
			return s.applyWithOptions(savedProfile{Config: next, Preferences: s.preferences}, true, false)
		})
		if err != nil {
			t.Fatal(err)
		}
		state := awaitJobIdle(t, &s.jobs)
		if state.ID != job.ID || state.SettingsSaved || state.Recovery != "static-ip-draft-available" || !strings.Contains(state.Error, "静态 IP 基准未保存") || !strings.Contains(state.Error, "代理连接失败") {
			t.Fatalf("unexpected default failure state: %+v", state)
		}
		saved, err := loadConfig(s.configPath)
		if err != nil || saved.ExitPolicy != baseCfg.ExitPolicy {
			t.Fatalf("default failed save mutated config: %+v %v", saved.ExitPolicy, err)
		}
	})

	t.Run("mainland static IP is never accepted even as offline draft", func(t *testing.T) {
		next := baseCfg
		next.ExitPolicy = cage.ExitPolicy{Mode: "static", Scope: "country", IP: "223.5.5.5"}
		_, err := s.submitJob("settings", func() error {
			return s.applyWithOptions(savedProfile{Config: next, Preferences: s.preferences}, true, true)
		})
		if err != nil {
			t.Fatal(err)
		}
		state := awaitJobIdle(t, &s.jobs)
		if state.SettingsSaved || state.Recovery != "" || !strings.Contains(state.Error, "静态 IP 基准未保存") {
			t.Fatalf("mainland static IP draft was not hard-rejected: %+v", state)
		}
		saved, err := loadConfig(s.configPath)
		if err != nil || saved.ExitPolicy != baseCfg.ExitPolicy {
			t.Fatalf("mainland draft mutated config: %+v %v", saved.ExitPolicy, err)
		}
	})

	t.Run("explicit draft save persists pending baseline while staying strictly fail-closed", func(t *testing.T) {
		// Agent dry_run validates save_static_draft without writing.
		if _, err := AgentCall(context.Background(), s.configPath, "settings.patch", json.RawMessage(`{"patch":{"exit_policy":{"mode":"static","ip":"8.8.8.8"}},"dry_run":true,"save_static_draft":true}`)); err != nil {
			t.Fatalf("agent dry_run with save_static_draft failed: %v", err)
		}
		afterDryRun, _ := loadConfig(s.configPath)
		if afterDryRun.ExitPolicy != baseCfg.ExitPolicy {
			t.Fatalf("dry_run modified config: %+v", afterDryRun.ExitPolicy)
		}

		// Real Agent settings.patch with save_static_draft=true saves baseline and keeps network closed.
		raw, err := AgentCall(context.Background(), s.configPath, "settings.patch", json.RawMessage(`{"patch":{"exit_policy":{"mode":"static","ip":"8.8.8.8"}},"save_static_draft":true}`))
		if err != nil {
			t.Fatalf("agent settings.patch submission failed: %v", err)
		}
		var accepted Job
		if err := json.Unmarshal(raw, &accepted); err != nil || accepted.ID == "" {
			t.Fatalf("unexpected accepted job: %s %v", raw, err)
		}
		state := awaitJobIdle(t, &s.jobs)
		if !state.SettingsSaved || state.Message != "已保存待验证的静态 IP，外网保持关闭" || !strings.Contains(state.Error, "已保存待验证的静态 IP 基准（8.8.8.8）") || !strings.Contains(state.Error, "外网保持关闭") {
			t.Fatalf("unexpected draft save job state: %+v", state)
		}
		saved, err := loadConfig(s.configPath)
		if err != nil || saved.ExitPolicy.Mode != "static" || saved.ExitPolicy.IP != "8.8.8.8" {
			t.Fatalf("static IP draft was not saved to config: %+v %v", saved.ExitPolicy, err)
		}
		store, err := managedproxy.Load(s.profile)
		if err != nil || store.ExitPolicies[selectedNode].Mode != "static" || store.ExitPolicies[selectedNode].IP != "8.8.8.8" {
			t.Fatalf("static IP draft was not saved per node: %+v %v", store.ExitPolicies, err)
		}
		s.mu.Lock()
		loc, cachedCount := s.location, len(s.locations)
		s.mu.Unlock()
		if loc != nil || cachedCount != 0 {
			t.Fatalf("manual static IP draft was falsely recorded as live observation evidence: loc=%+v cached=%d", loc, cachedCount)
		}
		// Subsequent exit check MUST still fail closed without live country verification.
		if checkState, _ := s.checkExitPolicy(context.Background(), saved, "8.8.8.8"); checkState != cage.CheckUnknown {
			t.Fatalf("saved static draft bypassed live country verification: state=%v", checkState)
		}
	})
}
