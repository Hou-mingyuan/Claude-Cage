package managedproxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func TestRelayGroupSourceAndCustomPoolRuntime(t *testing.T) {
	profile := t.TempDir()
	s, err := Load(profile)
	if err != nil {
		t.Fatal(err)
	}
	airportYAML := `proxies:
  - {name: hk-1, type: http, server: 198.51.100.1, port: 8080}
  - {name: hk-2, type: http, server: 198.51.100.2, port: 8080, dialer-proxy: hk-1}
  - {name: jp-1, type: http, server: 198.51.100.3, port: 8080}
`
	exitYAML := `proxies:
  - {name: home-exit, type: socks5, server: 203.0.113.10, port: 1080, password: secret}
`
	if err := s.Import("airport", "", []byte(airportYAML), "airport", 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Import("home", "", []byte(exitYAML), "home", 0, ""); err != nil {
		t.Fatal(err)
	}
	hk1, hk2, jp1 := s.Sources[0].Nodes[0].ID, s.Sources[0].Nodes[1].ID, s.Sources[0].Nodes[2].ID
	exitID := s.Sources[1].Nodes[0].ID

	// 1. Source-based fallback relay group -> fixed exit.
	if err := s.SetRouteHops([]RouteHop{
		{Relay: &RelayGroup{Strategy: RelayStrategyFallback, SourceID: "airport"}},
		{NodeID: exitID},
	}); err != nil {
		t.Fatal(err)
	}
	if s.Selected != exitID || len(s.Chain) != 0 || len(s.RouteHops) != 2 {
		t.Fatalf("unexpected store route state: selected=%q chain=%v hops=%+v", s.Selected, s.Chain, s.RouteHops)
	}
	hops, err := s.ResolveRoute()
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 2 || hops[0].GroupName != "CAGE-RELAY-0" || !reflect.DeepEqual(hops[0].Candidates, []string{hk1, hk2, jp1}) || hops[0].CandidateCount != 3 {
		t.Fatalf("unexpected resolved hops: %+v", hops)
	}

	raw, err := s.Runtime(profile)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Proxies     []map[string]any `yaml:"proxies"`
		ProxyGroups []map[string]any `yaml:"proxy-groups"`
		Rules       []string         `yaml:"rules"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.ProxyGroups) != 2 {
		t.Fatalf("want 2 proxy-groups, got %d: %+v", len(cfg.ProxyGroups), cfg.ProxyGroups)
	}
	if cfg.ProxyGroups[0]["name"] != Group || !reflect.DeepEqual(cfg.ProxyGroups[0]["proxies"], []any{exitID}) {
		t.Fatalf("CAGE group should stay pinned to exit: %+v", cfg.ProxyGroups[0])
	}
	relayPG := cfg.ProxyGroups[1]
	if relayPG["name"] != "CAGE-RELAY-0" || relayPG["type"] != "fallback" {
		t.Fatalf("unexpected relay group: %+v", relayPG)
	}
	if !reflect.DeepEqual(relayPG["proxies"], []any{hk1, hk2, jp1}) {
		t.Fatalf("unexpected relay group members: %+v", relayPG["proxies"])
	}
	byName := map[string]map[string]any{}
	for _, p := range cfg.Proxies {
		byName[p["name"].(string)] = p
	}
	// Hop 0 candidates must have their imported dialer-proxy cleared.
	if _, hasDialer := byName[hk2]["dialer-proxy"]; hasDialer {
		t.Fatalf("hop 0 candidate hk2 retained imported dialer-proxy: %+v", byName[hk2])
	}
	// Exit must dial through CAGE-RELAY-0.
	if byName[exitID]["dialer-proxy"] != "CAGE-RELAY-0" {
		t.Fatalf("exit dialer-proxy = %v, want CAGE-RELAY-0", byName[exitID]["dialer-proxy"])
	}
	// Per-account route for hk2 must keep its own cloned route-<id> with original imported dialer-proxy.
	acctHK2, ok := byName["route-"+hk2]
	if !ok || acctHK2["dialer-proxy"] != hk1 {
		t.Fatalf("per-account route for hk2 did not preserve original dialer-proxy: %+v", acctHK2)
	}
	// Per-account route for exitID must keep its own cloned route-<id> without CAGE-RELAY-0.
	acctExit, ok := byName["route-"+exitID]
	if !ok || acctExit["dialer-proxy"] != nil {
		t.Fatalf("per-account route for exit leaked environment relay group: %+v", acctExit)
	}

	// 2. Custom ordered node pool with url-test strategy (reversed order preserved).
	if err := s.SetRouteHops([]RouteHop{
		{Relay: &RelayGroup{Strategy: RelayStrategyURLTest, NodeIDs: []string{jp1, hk1}}},
		{NodeID: exitID},
	}); err != nil {
		t.Fatal(err)
	}
	raw, err = s.Runtime(profile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ProxyGroups = nil
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	relayPG = cfg.ProxyGroups[1]
	if relayPG["type"] != "url-test" || relayPG["tolerance"] != relayURLTestTolerance {
		t.Fatalf("unexpected url-test group: %+v", relayPG)
	}
	if !reflect.DeepEqual(relayPG["proxies"], []any{jp1, hk1}) {
		t.Fatalf("custom pool order not preserved: %+v", relayPG["proxies"])
	}

	// 3. Persistence round-trip.
	if err := s.Save(profile); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(profile)
	if err != nil {
		t.Fatal(err)
	}
	loadedHops, err := loaded.ResolveRoute()
	if err != nil || len(loadedHops) != 2 || !reflect.DeepEqual(loadedHops[0].Candidates, []string{jp1, hk1}) {
		t.Fatalf("saved relay hops lost after reload: hops=%+v err=%v", loadedHops, err)
	}

	// 4. Same-source relay excludes the fixed exit automatically.
	if err := loaded.SetRouteHops([]RouteHop{
		{Relay: &RelayGroup{Strategy: RelayStrategyFallback, SourceID: "airport"}},
		{NodeID: jp1},
	}); err != nil {
		t.Fatal(err)
	}
	sameSourceHops, err := loaded.ResolveRoute()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sameSourceHops[0].Candidates, []string{hk1, hk2}) {
		t.Fatalf("same-source relay should exclude exit jp1, got %v", sameSourceHops[0].Candidates)
	}
}

func TestRelayGroupValidationBoundaries(t *testing.T) {
	s, _ := Load(t.TempDir())
	if err := s.Import("src1", "", []byte(`proxies:
  - {name: n1, type: http, server: 198.51.100.1, port: 8080}
  - {name: n2, type: http, server: 198.51.100.2, port: 8080}
  - {name: n3, type: http, server: 198.51.100.3, port: 8080}
`), "src1", 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Import("src2", "", []byte(`proxies:
  - {name: exit, type: socks5, server: 203.0.113.10, port: 1080}
`), "src2", 0, ""); err != nil {
		t.Fatal(err)
	}
	n1, n2, n3 := s.Sources[0].Nodes[0].ID, s.Sources[0].Nodes[1].ID, s.Sources[0].Nodes[2].ID
	exitID := s.Sources[1].Nodes[0].ID

	invalid := []struct {
		name string
		hops []RouteHop
	}{
		{"empty", nil},
		{"relay as final exit", []RouteHop{{Relay: &RelayGroup{Strategy: RelayStrategyFallback, SourceID: "src1"}}}},
		{"invalid strategy", []RouteHop{{Relay: &RelayGroup{Strategy: "load-balance", SourceID: "src1"}}, {NodeID: exitID}}},
		{"both source_id and node_ids", []RouteHop{{Relay: &RelayGroup{Strategy: RelayStrategyFallback, SourceID: "src1", NodeIDs: []string{n1, n2}}}, {NodeID: exitID}}},
		{"neither source_id nor node_ids", []RouteHop{{Relay: &RelayGroup{Strategy: RelayStrategyFallback}}, {NodeID: exitID}}},
		{"single node in custom pool on apply", []RouteHop{{Relay: &RelayGroup{Strategy: RelayStrategyFallback, NodeIDs: []string{n1}}}, {NodeID: exitID}}},
		{"single usable node in source on apply", []RouteHop{{Relay: &RelayGroup{Strategy: RelayStrategyFallback, SourceID: "src2"}}, {NodeID: n1}}},
		{"duplicate node in custom pool", []RouteHop{{Relay: &RelayGroup{Strategy: RelayStrategyFallback, NodeIDs: []string{n1, n1}}}, {NodeID: exitID}}},
		{"exit included in custom pool", []RouteHop{{Relay: &RelayGroup{Strategy: RelayStrategyFallback, NodeIDs: []string{n1, exitID}}}, {NodeID: exitID}}},
		{"duplicate source across hops", []RouteHop{
			{Relay: &RelayGroup{Strategy: RelayStrategyFallback, SourceID: "src1"}},
			{Relay: &RelayGroup{Strategy: RelayStrategyURLTest, SourceID: "src1"}},
			{NodeID: exitID},
		}},
		{"overlapping nodes across hops", []RouteHop{
			{Relay: &RelayGroup{Strategy: RelayStrategyFallback, NodeIDs: []string{n1, n2}}},
			{Relay: &RelayGroup{Strategy: RelayStrategyURLTest, NodeIDs: []string{n2, n3}}},
			{NodeID: exitID},
		}},
		{"both node_id and relay on same hop", []RouteHop{
			{NodeID: n1, Relay: &RelayGroup{Strategy: RelayStrategyFallback, SourceID: "src1"}},
			{NodeID: exitID},
		}},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			copyStore := s
			if err := copyStore.SetRouteHops(tc.hops); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}

	// Fixed-only hops collapse to Chain/Selected and leave RouteHops empty.
	copyStore := s
	if err := copyStore.SetRouteHops([]RouteHop{{NodeID: n1}, {NodeID: exitID}}); err != nil {
		t.Fatal(err)
	}
	if len(copyStore.RouteHops) != 0 || !reflect.DeepEqual(copyStore.Chain, []string{n1, exitID}) || copyStore.Selected != exitID {
		t.Fatalf("fixed hops should collapse to Chain: hops=%v chain=%v selected=%q", copyStore.RouteHops, copyStore.Chain, copyStore.Selected)
	}
}

func TestRelayGroupRefreshRelaxedPruning(t *testing.T) {
	s, _ := Load(t.TempDir())
	if err := s.Import("airport", "", []byte(`proxies:
  - {name: hk-1, type: http, server: 198.51.100.1, port: 8080}
  - {name: hk-2, type: http, server: 198.51.100.2, port: 8080}
  - {name: jp-1, type: http, server: 198.51.100.3, port: 8080}
`), "airport", 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Import("home", "", []byte(`proxies:
  - {name: exit, type: socks5, server: 203.0.113.10, port: 1080}
`), "home", 0, ""); err != nil {
		t.Fatal(err)
	}
	hk1, hk2, jp1 := s.Sources[0].Nodes[0].ID, s.Sources[0].Nodes[1].ID, s.Sources[0].Nodes[2].ID
	exitID := s.Sources[1].Nodes[0].ID

	if err := s.SetRouteHops([]RouteHop{
		{Relay: &RelayGroup{Strategy: RelayStrategyFallback, NodeIDs: []string{hk1, hk2, jp1}}},
		{NodeID: exitID},
	}); err != nil {
		t.Fatal(err)
	}

	// Refresh removes hk-2: Option A prunes hk-2 and keeps [hk1, jp1].
	pruned, err := s.ImportWithPruned("airport", "", []byte(`proxies:
  - {name: hk-1, type: http, server: 198.51.100.1, port: 8080}
  - {name: jp-1, type: http, server: 198.51.100.3, port: 8080}
  - {name: sg-1, type: http, server: 198.51.100.4, port: 8080}
`), "airport", 0, "")
	if err != nil {
		t.Fatalf("relaxed refresh should succeed: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1", pruned)
	}
	if !reflect.DeepEqual(s.RouteHops[0].Relay.NodeIDs, []string{hk1, jp1}) {
		t.Fatalf("remaining candidates = %v, want [%s %s]", s.RouteHops[0].Relay.NodeIDs, hk1, jp1)
	}

	// Refresh leaves only hk-1 (1 node): still survives without breaking working route.
	pruned, err = s.ImportWithPruned("airport", "", []byte(`proxies:
  - {name: hk-1, type: http, server: 198.51.100.1, port: 8080}
`), "airport", 0, "")
	if err != nil {
		t.Fatalf("refresh leaving 1 candidate should survive: %v", err)
	}
	if pruned != 1 || !reflect.DeepEqual(s.RouteHops[0].Relay.NodeIDs, []string{hk1}) {
		t.Fatalf("unexpected state after second prune: pruned=%d ids=%v", pruned, s.RouteHops[0].Relay.NodeIDs)
	}

	// Refresh removes ALL remaining candidates: must reject and preserve old config.
	if _, err := s.ImportWithPruned("airport", "", []byte(`proxies:
  - {name: totally-new, type: http, server: 198.51.100.9, port: 8080}
`), "airport", 0, ""); err == nil {
		t.Fatal("refresh emptying relay pool should be rejected")
	}
	if !reflect.DeepEqual(s.RouteHops[0].Relay.NodeIDs, []string{hk1}) || s.Sources[0].Nodes[0].Name != "hk-1" {
		t.Fatal("rejected refresh mutated store")
	}

	// Refresh removing fixed exit must still be rejected.
	if _, err := s.ImportWithPruned("home", "", []byte(`proxies:
  - {name: other-exit, type: socks5, server: 203.0.113.11, port: 1080}
`), "home", 0, ""); err == nil {
		t.Fatal("refresh removing fixed exit should be rejected")
	}
}

func findTestMihomo(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("CAGE_TEST_REAL_MIHOMO"); p != "" {
		if !filepath.IsAbs(p) {
			t.Fatal("CAGE_TEST_REAL_MIHOMO must be an absolute path to the test binary")
		}
		return p
	}
	if p, err := CorePath(); err == nil {
		return p
	}
	name := "mihomo"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	candidates := []string{
		filepath.Join("..", "..", "_output", "proxy-core", name),
		filepath.Join("..", "..", "_output", "bin", name),
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			abs, err := filepath.Abs(p)
			if err == nil {
				return abs
			}
		}
	}
	return ""
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startTestConnectProxy runs a minimal HTTP CONNECT + plain HTTP proxy for
// end-to-end Mihomo chaining and failover verification.
func startTestConnectProxy(t *testing.T, hits *atomic.Int32) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	directTransport := &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true,
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			if r.Method == http.MethodConnect {
				up, err := net.DialTimeout("tcp4", r.Host, 3*time.Second)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadGateway)
					return
				}
				hj, ok := w.(http.Hijacker)
				if !ok {
					up.Close()
					http.Error(w, "no hijack", http.StatusInternalServerError)
					return
				}
				conn, rw, err := hj.Hijack()
				if err != nil {
					up.Close()
					return
				}
				_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
				go func() {
					defer up.Close()
					defer conn.Close()
					_, _ = io.Copy(up, rw)
				}()
				go func() {
					defer up.Close()
					defer conn.Close()
					_, _ = io.Copy(conn, up)
				}()
				return
			}
			// Plain HTTP proxy request (used by Mihomo url-test / fallback probes).
			req := r.Clone(r.Context())
			req.RequestURI = ""
			resp, err := directTransport.RoundTrip(req)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			for k, vals := range resp.Header {
				for _, v := range vals {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	stop = func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = ln.Close()
		directTransport.CloseIdleConnections()
	}
	t.Cleanup(stop)
	return ln.Addr().String(), stop
}

func TestRelayGroupRealMihomoValidationAndFailover(t *testing.T) {
	mihomoPath := findTestMihomo(t)
	if mihomoPath == "" {
		t.Skip("set CAGE_TEST_REAL_MIHOMO or provide the bundled Mihomo binary")
	}
	profile := t.TempDir()
	s, err := Load(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Import("airport", "", []byte(`proxies:
  - {name: relay-1, type: http, server: 198.51.100.1, port: 8081}
  - {name: relay-2, type: http, server: 198.51.100.2, port: 8082}
`), "airport", 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Import("home", "", []byte(`proxies:
  - {name: exit-fixed, type: http, server: 203.0.113.10, port: 8083}
`), "home", 0, ""); err != nil {
		t.Fatal(err)
	}
	r1, r2 := s.Sources[0].Nodes[0].ID, s.Sources[0].Nodes[1].ID
	exitID := s.Sources[1].Nodes[0].ID

	// 1. Validate both fallback and url-test configs with real `mihomo -t`.
	for _, strategy := range []string{RelayStrategyFallback, RelayStrategyURLTest} {
		if err := s.SetRouteHops([]RouteHop{
			{Relay: &RelayGroup{Strategy: strategy, NodeIDs: []string{r1, r2}}},
			{NodeID: exitID},
		}); err != nil {
			t.Fatal(err)
		}
		cfgBytes, err := s.Runtime(profile)
		if err != nil {
			t.Fatal(err)
		}
		cfgPath := filepath.Join(t.TempDir(), strategy+".yaml")
		if err := os.WriteFile(cfgPath, cfgBytes, 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(mihomoPath, "-t", "-d", filepath.Dir(cfgPath), "-f", cfgPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("mihomo -t failed for %s: %v\n%s", strategy, err, string(out))
		}
	}

	// 2. Live end-to-end failover: relay-1 -> exit -> target, then stop relay-1 -> relay-2 -> exit -> target, then stop relay-2 -> fail closed.
	var r1Hits, r2Hits, exitHits, targetHits atomic.Int32
	r1Addr, stopR1 := startTestConnectProxy(t, &r1Hits)
	r2Addr, stopR2 := startTestConnectProxy(t, &r2Hits)
	exitAddr, _ := startTestConnectProxy(t, &exitHits)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/probe" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "via-fixed-exit")
	}))
	defer target.Close()

	// Allocate a free port for Mihomo mixed-port.
	probeLn, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mixedPort := probeLn.Addr().(*net.TCPAddr).Port
	probeLn.Close()

	// Keep the Unix socket path short on every supported host.
	sockDir, err := os.MkdirTemp("", "relay-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "c.sock")

	s.Port = mixedPort
	if err := s.SetRouteHops([]RouteHop{
		{Relay: &RelayGroup{Strategy: RelayStrategyFallback, NodeIDs: []string{r1, r2}}},
		{NodeID: exitID},
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := s.runtimeWithSocket(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	// Point the generated proxies at our local test proxies and probe URL (test-only harness).
	splitHostPort := func(hp string) (string, int) {
		h, p, _ := net.SplitHostPort(hp)
		var port int
		_, _ = fmt.Sscanf(p, "%d", &port)
		return h, port
	}
	r1Host, r1Port := splitHostPort(r1Addr)
	r2Host, r2Port := splitHostPort(r2Addr)
	eHost, ePort := splitHostPort(exitAddr)
	for _, item := range doc["proxies"].([]any) {
		m := item.(map[string]any)
		switch m["name"] {
		case r1:
			m["server"], m["port"] = r1Host, r1Port
		case r2:
			m["server"], m["port"] = r2Host, r2Port
		case exitID:
			m["server"], m["port"] = eHost, ePort
		}
	}
	for _, item := range doc["proxy-groups"].([]any) {
		g := item.(map[string]any)
		if g["name"] == "CAGE-RELAY-0" {
			g["url"] = target.URL + "/probe"
			g["interval"] = 1
			g["timeout"] = 1000
		}
	}
	liveCfgPath := filepath.Join(sockDir, "config.yaml")
	liveBytes, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(liveCfgPath, liveBytes, 0600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, mihomoPath, "-d", sockDir, "-f", liveCfgPath)
	cmd.Env = coreEnvironment(sockDir)
	var mihomoLogs lockedBuffer
	cmd.Stdout = &mihomoLogs
	cmd.Stderr = &mihomoLogs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})

	// Wait for Mihomo ApplyConfig to finish completely (control API, mixed-port,
	// unified-delay, routing rules, CAGE group, and CAGE-RELAY-0 group).
	// Mihomo opens the Unix controller socket before ApplyConfig finishes loading
	// rules and activating the tunnel; sending proxy traffic as soon as only
	// /proxies/CAGE-RELAY-0 responds can race with rule/tunnel activation and
	// receive HTTP 502 on slower hosts.
	hops, _ := s.ResolveRoute()
	deadline := time.Now().Add(8 * time.Second)
	for {
		var cfg struct {
			Port         int  `json:"mixed-port"`
			UnifiedDelay bool `json:"unified-delay"`
		}
		var rules struct {
			Rules []any `json:"rules"`
		}
		var mainGroup struct {
			Now string `json:"now"`
		}
		var pg struct {
			Type string   `json:"type"`
			All  []string `json:"all"`
			Now  string   `json:"now"`
		}
		if apiOnSocket(ctx, sockPath, "GET", "/configs", nil, &cfg) == nil &&
			cfg.Port == mixedPort &&
			cfg.UnifiedDelay &&
			apiOnSocket(ctx, sockPath, "GET", "/rules", nil, &rules) == nil &&
			len(rules.Rules) > 0 &&
			apiOnSocket(ctx, sockPath, "GET", "/proxies/"+Group, nil, &mainGroup) == nil &&
			mainGroup.Now == exitID &&
			apiOnSocket(ctx, sockPath, "GET", "/proxies/CAGE-RELAY-0", nil, &pg) == nil {
			if !relayGroupMatches(pg.Type, pg.All, hops[0]) {
				t.Fatalf("relayGroupMatches failed on live mihomo response: %+v\nmihomo logs:\n%s", pg, mihomoLogs.String())
			}
			if pg.Now == r1 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("mihomo did not finish startup readiness\nmihomo logs:\n%s", mihomoLogs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	proxyURL, err := url.Parse(fmt.Sprintf("http://cage:%s@127.0.0.1:%d", s.Auth, mixedPort))
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true},
		Timeout:   4 * time.Second,
	}
	getTarget := func() (string, error) {
		resp, err := client.Get(target.URL + "/hello")
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("status %d: %s", resp.StatusCode, string(b))
		}
		return string(b), nil
	}

	// Phase 1: traffic goes through relay-1 -> exit -> target.
	body, err := getTarget()
	if err != nil || body != "via-fixed-exit" {
		t.Fatalf("phase 1 failed: body=%q err=%v\nmihomo logs:\n%s", body, err, mihomoLogs.String())
	}
	if r1Hits.Load() == 0 || exitHits.Load() == 0 {
		t.Fatalf("phase 1 did not traverse relay1 and exit: r1=%d exit=%d\nmihomo logs:\n%s", r1Hits.Load(), exitHits.Load(), mihomoLogs.String())
	}

	// Phase 2: stop relay-1; Mihomo fallback must automatically switch to relay-2 while keeping exit!
	stopR1()
	exitBefore := exitHits.Load()
	r2Before := r2Hits.Load()
	var phase2Err error
	for i := 0; i < 30; i++ {
		var pg struct {
			Now string `json:"now"`
		}
		_ = apiOnSocket(ctx, sockPath, "GET", "/proxies/CAGE-RELAY-0", nil, &pg)
		if pg.Now == r2 {
			body, phase2Err = getTarget()
			if phase2Err == nil && body == "via-fixed-exit" {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if phase2Err != nil || r2Hits.Load() <= r2Before || exitHits.Load() <= exitBefore {
		t.Fatalf("phase 2 failover did not switch to relay2->exit: err=%v r2=%d->%d exit=%d->%d\nmihomo logs:\n%s",
			phase2Err, r2Before, r2Hits.Load(), exitBefore, exitHits.Load(), mihomoLogs.String())
	}

	// Phase 3: stop relay-2 as well; requests must fail strictly with 0 direct connections to target.
	stopR2()
	time.Sleep(1500 * time.Millisecond)
	hitsBefore := targetHits.Load()
	if _, err := getTarget(); err == nil {
		t.Fatalf("phase 3 succeeded after all relays stopped (unexpected direct fallback)\nmihomo logs:\n%s", mihomoLogs.String())
	}
	if targetHits.Load() != hitsBefore {
		t.Fatalf("phase 3 leaked direct traffic to target: hits %d -> %d\nmihomo logs:\n%s", hitsBefore, targetHits.Load(), mihomoLogs.String())
	}
}
