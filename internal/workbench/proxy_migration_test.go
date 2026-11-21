package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
)

func TestLegacyLocalProxyCanBeReplacedWithoutApplyingIt(t *testing.T) {
	s := testServer(t)
	store, err := managedproxy.Load(s.profile)
	if err != nil {
		t.Fatal(err)
	}
	oldID := store.Selected
	store.Sources[0].Nodes[0].Config["server"] = "127.0.0.1"
	if err = store.Save(s.profile); err != nil {
		t.Fatal(err)
	}
	view := request(s, "GET", "/api/proxy", s.origin, "", true)
	var status struct {
		State   string
		Route   []string
		Sources []struct {
			Nodes []struct {
				Blocked string `json:"blocked_reason"`
			}
		}
	}
	if err = json.Unmarshal(view.Body.Bytes(), &status); err != nil || status.State != "migration_required" ||
		!reflect.DeepEqual(status.Route, []string{oldID}) || len(status.Sources) != 1 || len(status.Sources[0].Nodes) != 1 || status.Sources[0].Nodes[0].Blocked == "" {
		t.Fatalf("missing preserved route or migration guidance: %s", view.Body)
	}
	if err := s.validateAccountNode(oldID); !errors.Is(err, managedproxy.ErrLocalProxyNode) {
		t.Fatal("account could select legacy relay", err)
	}
	delay := request(s, "POST", "/api/proxy/delay", s.origin, `{"node_id":"`+oldID+`"}`, true)
	if delay.Code != http.StatusBadRequest || !strings.Contains(delay.Body.String(), "本机") {
		t.Fatalf("legacy speed test: %d %s", delay.Code, delay.Body)
	}
	validate, start := validateProxyCore, startProxyCore
	t.Cleanup(func() { validateProxyCore, startProxyCore = validate, start })
	validateProxyCore = func(_ context.Context, profile string, p managedproxy.Store) error {
		_, err := p.Runtime(profile)
		return err
	}
	starts := 0
	startProxyCore = func(context.Context, string, managedproxy.Store) error { starts++; return nil }
	if err := s.changeProxy("restart", proxyInput{}, nil); !errors.Is(err, managedproxy.ErrLocalProxyNode) {
		t.Fatal("restart accepted legacy relay", err)
	}
	err = s.changeProxy("import", proxyInput{Name: "remote nodes", Content: `proxies:
- {name: transit, type: socks5, server: entry.example, port: 1080}
- {name: exit, type: http, server: exit.example, port: 443}`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	after, err := managedproxy.Load(s.profile)
	if err != nil || starts != 0 || after.Selected != oldID || after.Revision != "" || len(after.Sources) != 2 {
		t.Fatal("import changed or reactivated saved route", err, starts, after.Selected)
	}
	remote := after.Sources[1].Nodes
	chain := []string{remote[0].ID, remote[1].ID}
	if err = s.changeProxy("route", proxyInput{NodeIDs: chain}, nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	after, err = managedproxy.Applied(s.profile, cfg.ProxyURL, cfg.ProxyRevision)
	if err != nil || starts != 1 || !reflect.DeepEqual(after.Chain, chain) || len(after.Sources) != 2 {
		t.Fatal("explicit migration did not preserve old source and apply new chain", err, starts)
	}
}

func TestStartupRevokesGateWhenStaleCoreHadLocalAccountRoute(t *testing.T) {
	s, gate := interfaceMonitorServer(t)
	grantTestGate(t, gate)
	path := filepath.Join(managedproxy.Dir(s.profile), "config.yaml")
	if err := os.WriteFile(path, []byte("interface-name: en0\nproxies: [{name: old-account, type: socks5, server: localhost, port: 10808}]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.retireLocalProxyCore(); err != nil {
		t.Fatal(err)
	}
	status := gate.Status()
	if status.Mode != cage.GateClosed || !strings.Contains(status.Reason, "本机") || s.networkPaused {
		t.Fatal("inherited core was not revoked without changing manual intent", status.Mode, status.Reason)
	}
}

func TestNetworkMonitorRetiresOnlyTheObservedLegacyRuntime(t *testing.T) {
	for _, replace := range []bool{false, true} {
		s, gate := interfaceMonitorServer(t)
		grantTestGate(t, gate)
		path := filepath.Join(managedproxy.Dir(s.profile), "config.yaml")
		stale := []byte("interface-name: en0\nproxies: [{name: legacy, type: socks5, server: localhost, port: 10808}]\n")
		if err := managedproxy.Write(path, stale); err != nil {
			t.Fatal(err)
		}
		observed := s.observeProxyInterface(context.Background())
		if !errors.Is(observed.bindingErr, managedproxy.ErrLocalProxyNode) {
			t.Fatal("stale generated configuration not observed", observed.bindingErr)
		}
		if replace {
			if err := managedproxy.Write(path, []byte("interface-name: en0\nproxies: [{name: remote, type: socks5, server: entry.example, port: 443}]\n")); err != nil {
				t.Fatal(err)
			}
		}
		s.op.Lock()
		s.applyProxyInterfaceObservation(observed)
		s.op.Unlock()
		_, err := os.Stat(path)
		if replace {
			if gate.Status().Mode != cage.GateOpen || s.proxyNetworkLost || err != nil {
				t.Fatal("stale observation retired a replacement core")
			}
		} else if gate.Status().Mode != cage.GateClosed || !s.proxyNetworkLost || !os.IsNotExist(err) {
			t.Fatal("legacy runtime or gate survived")
		}
	}
}
