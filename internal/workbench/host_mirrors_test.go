package workbench

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
)

func withMirrorFixture(t *testing.T, base string) {
	t.Helper()
	original := debianMirrorCandidates
	t.Cleanup(func() { debianMirrorCandidates = original })
	debianMirrorCandidates = nil
	for _, c := range original {
		debianMirrorCandidates = append(debianMirrorCandidates, struct{ ID, Name, URL string }{c.ID, c.Name, base + "/" + c.ID})
	}
}

// Each candidate is served by a fixture whose throughput differs; the
// recommendation follows the measured rates, not a fixed order.
func TestProbeDebianMirrorsRecommendation(t *testing.T) {
	var mu sync.Mutex
	delay := map[string]time.Duration{}
	unavailable := map[string]bool{}
	served := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.Split(r.URL.Path, "/")[1]
		mu.Lock()
		served[id] = r.URL.Path
		wait, down := delay[id], unavailable[id]
		mu.Unlock()
		if down {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		chunk := make([]byte, 64<<10)
		for i := 0; i < 8; i++ {
			time.Sleep(wait)
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	}))
	defer server.Close()
	withMirrorFixture(t, server.URL)
	set := func(nju, ustc, official time.Duration, down ...string) {
		mu.Lock()
		defer mu.Unlock()
		delay["nju"], delay["ustc"], delay["official"] = nju, ustc, official
		clear(unavailable)
		for _, id := range down {
			unavailable[id] = true
		}
	}
	// Overseas exit: the official CDN is fastest; keep the official-first default.
	set(40*time.Millisecond, 0, 0, "ustc")
	result := probeDebianMirrors(context.Background(), server.Client(), "trixie", "arm64")
	if len(result.Samples) != 3 || result.Recommended != "auto" || result.Domestic != "nju" {
		t.Fatalf("%+v", result)
	}
	for _, sample := range result.Samples {
		switch sample.ID {
		case "ustc":
			if sample.Error != "HTTP 503" || sample.BytesPerSecond != 0 {
				t.Fatalf("unavailable mirror not reported: %+v", sample)
			}
		default:
			if sample.Error != "" || sample.Bytes != 8*64<<10 || sample.BytesPerSecond <= 0 {
				t.Fatalf("sample not measured: %+v", sample)
			}
		}
	}
	if !strings.HasSuffix(served["official"], "/dists/trixie/main/binary-arm64/Packages.gz") {
		t.Fatalf("wrong index path: %s", served["official"])
	}
	// Domestic connection: the faster mirror is recommended by name, so the
	// guest puts it first even though nju precedes it in the candidate list.
	set(50*time.Millisecond, 0, 60*time.Millisecond)
	if result = probeDebianMirrors(context.Background(), server.Client(), "trixie", "arm64"); result.Recommended != "ustc" || result.Domestic != "ustc" {
		t.Fatalf("fastest mirror not recommended: %+v", result)
	}
	set(0, 50*time.Millisecond, 60*time.Millisecond)
	if result = probeDebianMirrors(context.Background(), server.Client(), "trixie", "arm64"); result.Recommended != "nju" {
		t.Fatalf("fastest mirror not recommended: %+v", result)
	}
	// A marginal difference is noise; the default stays.
	set(20*time.Millisecond, 60*time.Millisecond, 22*time.Millisecond)
	if result = probeDebianMirrors(context.Background(), server.Client(), "trixie", "arm64"); result.Recommended != "auto" || result.Domestic != "nju" {
		t.Fatalf("marginal difference changed the default: %+v", result)
	}
	for _, value := range []string{result.Recommended, result.Domestic} {
		if !cage.ValidDebianMirror(value) {
			t.Fatalf("probe returned a value the guest rejects: %q", value)
		}
	}
}

func TestProbeDebianMirrorsWithoutAnyReachableSource(t *testing.T) {
	withMirrorFixture(t, "http://127.0.0.1:1")
	client := &http.Client{Timeout: time.Second}
	result := probeDebianMirrors(context.Background(), client, "trixie", "amd64")
	if result.Recommended != "auto" || result.Domestic != "nju" || len(result.Samples) != 3 || result.Samples[0].Error == "" {
		t.Fatalf("%+v", result)
	}
}

// A saved choice answers at once: no proxy is required and no candidate is
// contacted, so retrying an install does not repeat the measurement.
func TestHostDebianMirrorsSkipsMeasurementWhenDecided(t *testing.T) {
	contacted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { contacted = true }))
	defer server.Close()
	withMirrorFixture(t, server.URL)
	profile := t.TempDir()
	s := &Server{profile: profile, configPath: filepath.Join(profile, "missing.json"), preferences: Preferences{Keyboard: "mac", DebianMirror: "ustc"}}
	recorder := httptest.NewRecorder()
	s.hostDebianMirrors(recorder, httptest.NewRequest(http.MethodGet, "/api/host/debian-mirrors", nil))
	var got mirrorProbe
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil || recorder.Code != http.StatusOK {
		t.Fatalf("%d %s", recorder.Code, recorder.Body)
	}
	if !got.Decided || got.Preference != "ustc" || len(got.Samples) != 0 || contacted {
		t.Fatalf("saved choice measured again: %+v contacted=%v", got, contacted)
	}
	// Without a saved choice the measurement needs the applied Cage proxy.
	s.preferences.DebianMirror = ""
	recorder = httptest.NewRecorder()
	s.hostDebianMirrors(recorder, httptest.NewRequest(http.MethodGet, "/api/host/debian-mirrors", nil))
	if recorder.Code == http.StatusOK || contacted {
		t.Fatalf("measured without the Cage proxy: %d %s", recorder.Code, recorder.Body)
	}
}

func TestChooseDebianMirrorPersistsOnlyValidChoices(t *testing.T) {
	profile := t.TempDir()
	s := &Server{profile: profile, preferences: Preferences{Keyboard: "mac", BrowserHomeURL: "https://example.com/"}}
	post := func(value string) int {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/host/debian-mirrors", strings.NewReader(`{"preference":"`+value+`"}`))
		request.Header.Set("Content-Type", "application/json")
		s.chooseDebianMirror(recorder, request)
		return recorder.Code
	}
	if code := post("fastest"); code != http.StatusBadRequest || s.preferences.DebianMirror != "" {
		t.Fatalf("invalid choice accepted: %d", code)
	}
	if code := post("ustc"); code != http.StatusOK || s.preferences.DebianMirror != "ustc" {
		t.Fatalf("choice not saved: %d %+v", code, s.preferences)
	}
	data, err := os.ReadFile(filepath.Join(profile, "workbench.json"))
	var saved Preferences
	if err != nil || json.Unmarshal(data, &saved) != nil || saved.DebianMirror != "ustc" || saved.BrowserHomeURL != "https://example.com/" {
		t.Fatalf("saved preferences: %s %v", data, err)
	}
	// A failed write leaves the previous choice in effect.
	blocked := filepath.Join(profile, "blocked")
	if err := os.WriteFile(blocked, nil, 0600); err != nil {
		t.Fatal(err)
	}
	s.profile = blocked
	if code := post("official"); code == http.StatusOK || s.preferences.DebianMirror != "ustc" {
		t.Fatalf("failed save changed the choice: %d %+v", code, s.preferences)
	}
}

// Every sample goes to the selected proxy as an unresolved CONNECT target,
// one after another; nothing is dialed directly or resolved on the host.
func TestMirrorProbeClientUsesOnlyTheCageProxy(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var mu sync.Mutex
	var targets []string
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			request, err := http.ReadRequest(bufio.NewReader(conn))
			if err == nil {
				mu.Lock()
				targets = append(targets, request.Method+" "+request.Host)
				mu.Unlock()
			}
			conn.Write([]byte("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n"))
			conn.Close()
		}
	}()
	cfg := cage.DefaultConfig()
	cfg.ProxyURL = "http://" + listener.Addr().String()
	client, closeIdle, err := mirrorProbeClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer closeIdle()
	result := probeDebianMirrors(context.Background(), client, "trixie", "amd64")
	mu.Lock()
	defer mu.Unlock()
	want := []string{"CONNECT mirrors.nju.edu.cn:443", "CONNECT mirrors.ustc.edu.cn:443", "CONNECT deb.debian.org:443"}
	if strings.Join(targets, "\n") != strings.Join(want, "\n") {
		t.Fatalf("proxy saw %q, want %q", targets, want)
	}
	for _, sample := range result.Samples {
		if sample.Error == "" {
			t.Fatalf("refused proxy still produced a sample: %+v", sample)
		}
	}
	if result.Recommended != "auto" {
		t.Fatalf("%+v", result)
	}
}

func TestDebianMirrorPreferenceValidation(t *testing.T) {
	settings := func(value string) Settings {
		return Settings{Preferences: Preferences{Keyboard: "mac", DebianMirror: value}, DNSURL: "https://1.1.1.1/dns-query", Timezone: "Etc/UTC", Locale: "en_US.UTF-8", BrowserLanguage: "en-US"}
	}
	for _, value := range []string{"", "auto", "official", "nju", "ustc"} {
		if _, _, err := proposedSettings(settings(value), cage.DefaultConfig(), t.TempDir()); err != nil {
			t.Fatalf("%q rejected: %v", value, err)
		}
	}
	for _, value := range []string{"domestic", "fastest"} {
		if _, _, err := proposedSettings(settings(value), cage.DefaultConfig(), t.TempDir()); err == nil {
			t.Fatalf("invalid mirror preference %q accepted", value)
		}
	}
}

// Opt-in: measure the real candidates through the proxy of a profile.
// CAGE_MIRROR_PROBE_CONFIG=<profile>/config.json go test -run RealProxy -v
func TestProbeDebianMirrorsThroughRealProxy(t *testing.T) {
	path := os.Getenv("CAGE_MIRROR_PROBE_CONFIG")
	if path == "" {
		t.Skip("set CAGE_MIRROR_PROBE_CONFIG to a config.json")
	}
	cfg, err := cage.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	client, closeIdle, err := mirrorProbeClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer closeIdle()
	result := probeDebianMirrors(context.Background(), client, "trixie", debianGuestArch())
	for _, sample := range result.Samples {
		t.Logf("%-12s %-22s %8.0f KB/s  %7d bytes  %s", sample.ID, sample.Host, sample.BytesPerSecond/1024, sample.Bytes, sample.Error)
	}
	t.Logf("recommended: %s", result.Recommended)
}
