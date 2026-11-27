package workbench

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
)

func TestMaintenanceRequiresAuthenticationAndSoftwareSource(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("installer credential reached software origin")
		}
		_, _ = io.WriteString(w, "package bytes")
	}))
	defer upstream.Close()
	destination, _ := url.Parse(upstream.URL)
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "deb.debian.org:80" && address != "deb.nodesource.com:80" {
			return nil, errors.New("unexpected destination")
		}
		return (&net.Dialer{}).DialContext(ctx, network, destination.Host)
	}
	proxy := &maintenanceProxy{token: "fixture-only", ctx: context.Background(), dial: dial, connections: map[net.Conn]bool{}, transport: &http.Transport{Proxy: nil, DialContext: dial}}
	server := httptest.NewServer(proxy)
	defer server.Close()
	defer proxy.close()
	s, gate := grantedToolUpdateServer(t)
	_, _, recoveryErr := parseGuestConnection([]byte("{\"token\":\"fixture\"}\npassword\n{\"updating\":true}"))
	if !errors.Is(recoveryErr, errGuestRecoveryRequired) || !guestComponentsUnavailable(recoveryErr) {
		t.Fatal("interrupted start did not enter the component recovery path", recoveryErr)
	}
	if err := s.closeProxyGate(recoveryErr.Error()); err != nil {
		t.Fatal(err)
	}
	if conn, err := gate.DialContext(context.Background(), "tcp", "example.com:443"); err == nil {
		conn.Close()
		t.Fatal("component recovery allowed agent traffic")
	}
	for _, test := range []struct {
		target, method string
		auth           bool
		status         int
	}{
		{"http://deb.debian.org/debian/pool/file.deb", "GET", false, 407},
		{"http://example.com/private", "GET", true, 403},
		{"http://127.0.0.1/private", "GET", true, 403},
		{"http://deb.debian.org:8080/private", "GET", true, 403},
		{"http://deb.debian.org/debian/pool/file.deb", "POST", true, 405},
		{"http://deb.debian.org/debian/pool/file.deb", "GET", true, 200},
		{"http://deb.nodesource.com/node_24.x/dists/nodistro/InRelease", "GET", true, 200},
	} {
		proxyURL, _ := url.Parse(server.URL)
		if test.auth {
			proxyURL.User = url.UserPassword("cage-install", "fixture-only")
		}
		transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
		client := &http.Client{Transport: transport, Timeout: time.Second}
		request, _ := http.NewRequest(test.method, test.target, nil)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		transport.CloseIdleConnections()
		if response.StatusCode != test.status {
			t.Fatalf("%s: %d", test.target, response.StatusCode)
		}
		if test.status == 200 && string(body) != "package bytes" {
			t.Fatal("download bytes changed")
		}
	}
	if calls.Load() != 2 {
		t.Fatal("rejected request reached origin")
	}
	if gate.Status().Mode != cage.GateClosed {
		t.Fatal("maintenance changed business gate")
	}
}

func TestMaintenanceClosesTLSConnectionsAndRefusesBusinessHosts(t *testing.T) {
	proxy := &maintenanceProxy{token: "fixture", ctx: context.Background(), connections: map[net.Conn]bool{}, transport: &http.Transport{}}
	proxy.dial = func(context.Context, string, string) (net.Conn, error) {
		a, b := net.Pipe()
		go func() { defer b.Close(); _, _ = io.Copy(b, b) }()
		return a, nil
	}
	server := httptest.NewServer(proxy)
	defer server.Close()
	defer proxy.close()
	address, _ := url.Parse(server.URL)
	connection, err := net.Dial("tcp", address.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	auth := base64.StdEncoding.EncodeToString([]byte("cage-install:fixture"))
	_, _ = fmt.Fprintf(connection, "CONNECT deb.debian.org:443 HTTP/1.1\r\nHost: deb.debian.org:443\r\nProxy-Authorization: Basic %s\r\n\r\n", auth)
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, nil)
	if err != nil || response.StatusCode != 200 {
		t.Fatal(response, err)
	}
	_, _ = connection.Write([]byte("abc"))
	bytes := make([]byte, 3)
	if _, err = io.ReadFull(reader, bytes); err != nil || string(bytes) != "abc" {
		t.Fatal("tunnel failed", err)
	}
	proxy.close()
	if _, err = reader.ReadByte(); err == nil {
		t.Fatal("maintenance left a live tunnel")
	}
	for _, address := range []string{"claude.ai:443", "api.anthropic.com:443", "deb.debian.org:22", "127.0.0.1:443", "github.com.evil.test:443"} {
		if proxy.allowsDestination(address, true) {
			t.Fatal("business or non-HTTPS destination accepted", address)
		}
	}
	for _, host := range []string{"mirrors.nju.edu.cn", "mirrors.ustc.edu.cn", "cdn.npmmirror.com", "api.snapcraft.io", "storage.snapcraftcontent.com", "canonical-lgw01.cdn.snapcraftcontent.com", "canonical-lcy01.cdn.snapcraftcontent.com", "canonical-lcy02.cdn.snapcraftcontent.com", "canonical-bos01.cdn.snapcraftcontent.com", "cloudfront.cdn.snapcraftcontent.com", "codeload.github.com"} {
		if !proxy.allowsDestination(host+":443", true) || proxy.allowsDestination(host+":80", false) || proxy.allowsDestination(host+".evil.test:443", true) {
			t.Fatalf("unexpected mirror policy for %s", host)
		}
	}
}

func TestMaintenanceScriptUsesPrivateTemporaryConfiguration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the installer Bash script runs in the Linux guest, not the Windows host")
	}
	shell, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	command := exec.Command(shell, "-se")
	command.Stdin = strings.NewReader(maintenanceScript(`test -s "$CAGE_APT_CONFIG"
test "$http_proxy" = "$CAGE_INSTALL_PROXY"
test "$(umask)" = 0022
printf '%s' "$CAGE_APT_CONFIG"
	`, "fixture-token", false))
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(string(output)); !os.IsNotExist(err) {
		t.Fatal("maintenance credential was retained")
	}
}

func TestContainerMaintenanceKeepsAPTArchivesOutsideDefaultCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the installer Bash script runs in the Linux guest, not the Windows host")
	}
	shell, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	command := exec.Command(shell, "-se")
	command.Stdin = strings.NewReader(`sudo() {
  test "$*" = "install -d -m 0755 /var/cache/apt/install-archives"
}
` + maintenanceScript(`sed -n '/^Dir::Cache::archives /p' "$CAGE_APT_CONFIG"
`, "fixture-token", true))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run maintenance script: %v\n%s", err, output)
	}
	if got := strings.TrimSpace(string(output)); got != `Dir::Cache::archives "/var/cache/apt/install-archives";` {
		t.Fatalf("unexpected managed APT archive directory: %q", got)
	}
}

func TestMaintenanceScriptScopesThirdPartyDesktopAppAPTSources(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the installer Bash script runs in the Linux guest, not the Windows host")
	}
	shell, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	root := t.TempDir()
	sourcesDir := root + "/sources.list.d"
	appsDir := root + "/desktop-apps"
	codexID := desktopAppInstance("codex-app", "codex")
	otherID := desktopAppInstance("other-app", "other")
	if err := os.MkdirAll(sourcesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(appsDir+"/"+codexID, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"debian.sources":               "Types: deb\n",
		"desktop-app-" + codexID + ".list": "deb https://persistent.oaistatic.com/codex-app-prod/linux/deb stable main\n",
		"openai-vendor.list":           "deb https://persistent.oaistatic.com/codex-app-prod/linux/deb stable main\n",
		"desktop-app-" + otherID + ".list": "deb https://other.example.test/deb stable main\n",
	} {
		if err := os.WriteFile(sourcesDir+"/"+name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	record := fmt.Sprintf(`{"vendor_files":[%q]}`, sourcesDir+"/openai-vendor.list")
	if err := os.WriteFile(appsDir+"/"+codexID+"/app.json", []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}

	// Non-Codex maintenance excludes Codex's and other desktop apps' APT sources,
	// and cage_refresh_apt_sources picks up newly created Claude Desktop sources.
	script := fmt.Sprintf(`cage_apt_sources_dir=%q
cage_desktop_apps_dir=%q
cage_refresh_apt_sources
dir=$(sed -n 's/^Dir::Etc::sourceparts "\(.*\)";$/\1/p' "$CAGE_APT_CONFIG")
ls -1 "$dir" | sort | tr '\n' ','
printf '\n'
printf 'deb https://downloads.claude.ai/claude-desktop/apt/stable stable main\n' > "$cage_apt_sources_dir/claude-desktop.list"
cage_refresh_apt_sources
ls -1 "$dir" | sort | tr '\n' ','
printf '\n'
`, sourcesDir, appsDir)
	cmd := exec.Command(shell, "-se")
	cmd.Stdin = strings.NewReader(maintenanceScriptWithAPTScope(script, "fixture-token", false, ""))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("general maintenance failed: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 || lines[0] != "debian.sources," || lines[1] != "claude-desktop.list,debian.sources," {
		t.Fatalf("unexpected filtered sources for general maintenance: %q", out)
	}

	// Codex's own maintenance keeps Codex's list and vendor source while still
	// excluding other desktop applications' sources.
	codexScript := fmt.Sprintf(`cage_apt_sources_dir=%q
cage_desktop_apps_dir=%q
cage_refresh_apt_sources
dir=$(sed -n 's/^Dir::Etc::sourceparts "\(.*\)";$/\1/p' "$CAGE_APT_CONFIG")
ls -1 "$dir" | sort | tr '\n' ','
`, sourcesDir, appsDir)
	cmd = exec.Command(shell, "-se")
	cmd.Stdin = strings.NewReader(maintenanceScriptWithAPTScope(codexScript, "fixture-token", false, codexID))
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("codex maintenance failed: %v\n%s", err, out)
	}
	want := "claude-desktop.list,debian.sources,desktop-app-" + codexID + ".list,openai-vendor.list,"
	if got := strings.TrimSpace(string(out)); got != want {
		t.Fatalf("codex filtered sources = %q, want %q", got, want)
	}
}
