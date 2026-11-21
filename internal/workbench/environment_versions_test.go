package workbench

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
	"golang.org/x/crypto/ssh"
)

func TestGuestVersionSnapshotFiltersCodeServerOutput(t *testing.T) {
	names := []string{"python3", "python"}
	if runtime.GOOS == "windows" {
		names = []string{"python", "python3"} // python3.exe may be a Windows Store alias.
	}
	var python string
	for _, name := range names {
		path, err := exec.LookPath(name)
		if err == nil {
			version, err := exec.Command(path, "--version").Output()
			if err == nil && strings.HasPrefix(string(version), "Python 3.") {
				python = path
				break
			}
		}
	}
	if python == "" {
		t.Skip("Python 3 is required to exercise the Linux guest version probe")
	}
	_, script, _ := strings.Cut(guestVersionSnapshot, "\n")
	script = strings.TrimSuffix(script, "\nCAGE_VERSIONS")
	const mockTools = `
import subprocess,sys
def run(args, **kwargs):
    if args[0] == 'code-server':
        assert args == ['code-server', '--version', '--config', '/dev/null']
        return subprocess.CompletedProcess(args, int(sys.argv[2]), sys.argv[1])
    output = 'other tool version\nsecond line'
    return subprocess.CompletedProcess(args, 0, output)
subprocess.run = run
`
	const notice = "[2026-11-16T00:00:00.000Z] info  Wrote default config file to /home/guest/.config/code-server/config.yaml"
	const version = "4.145.0 2cb3ad3e258891fbb51001ba465c2348d5021644 with Code 1.145.0"
	for _, test := range []struct{ name, output, status, want string }{
		{"first run", notice + "\n" + version + "\n", "0", version},
		{"later run", version + "\n", "0", version},
		{"no version", notice + "\n", "0", ""},
		{"failed command", version + "\n", "1", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, err := exec.Command(python, "-c", mockTools+script, test.output, test.status).CombinedOutput()
			if err != nil {
				t.Fatalf("version probe: %v\n%s", err, output)
			}
			var snapshot struct{ Tools map[string]string }
			if err := json.Unmarshal(output, &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.Tools["code_server"] != test.want || snapshot.Tools["git"] != "other tool version" {
				t.Fatalf("unexpected versions: %v", snapshot.Tools)
			}
		})
	}
}

func TestGuestConnectionRequiresSuccessfulMatchingManifest(t *testing.T) {
	required := cage.RequiredGuestManifest()
	for _, name := range []string{"current", "missing", "old-revision", "old-protocol", "broken", "interrupted", "interrupted-current", "interrupted-during-read"} {
		t.Run(name, func(t *testing.T) {
			manifest := required
			if name == "old-revision" {
				manifest.Revision = "previous"
			}
			if name == "old-protocol" {
				manifest.Protocol++
			}
			if name == "interrupted" {
				manifest = cage.GuestManifest{Updating: true}
			}
			if name == "interrupted-current" {
				manifest.Updating = true
			}
			encoded, _ := json.Marshal(manifest)
			if name == "missing" {
				encoded = []byte("null")
			}
			if name == "broken" {
				encoded = []byte("{")
			}
			if name == "interrupted-during-read" {
				encoded = append(encoded, []byte("\nupdating")...)
			}
			credentials, current, err := parseGuestConnection(append([]byte("{\"token\":\"guest-test\"}\npassword\n"), encoded...))
			if name == "current" {
				if err != nil || current == nil || credentials.Token != "guest-test" || credentials.VNCPassword != "password" {
					t.Fatal(current, err)
				}
			} else if name == "old-revision" || name == "old-protocol" {
				if !errors.Is(err, errGuestUpdateRequired) {
					t.Fatalf("outdated guest did not require an update: %v", err)
				}
			} else if !errors.Is(err, errGuestRecoveryRequired) || errors.Is(err, errGuestUpdateRequired) || !guestComponentsUnavailable(err) {
				t.Fatalf("stale guest accepted: %v", err)
			}
			if err != nil && current != nil && current.Current() {
				t.Fatal("interrupted guest reported current")
			}
		})
	}
}

func TestGuestConnectionSnapshotDetectsInterruptedStart(t *testing.T) {
	for _, state := range []string{"current", "missing", "outdated", "interrupted", "interrupted-during-read"} {
		t.Run(state, func(t *testing.T) {
			dir := filepath.ToSlash(t.TempDir())
			if err := os.MkdirAll(filepath.Join(dir, ".config", "claude-cage"), 0700); err != nil {
				t.Fatal(err)
			}
			manifest := cage.RequiredGuestManifest()
			if state == "outdated" {
				manifest.Revision = "previous"
			}
			encoded, _ := json.Marshal(manifest)
			files := map[string][]byte{
				"browser-control.json":                 []byte(`{"token":"fixture"}`),
				".config/claude-cage/browser-password": []byte("password"),
			}
			if state != "missing" {
				files["components.json"] = encoded
			}
			if state == "interrupted" {
				files["components.updating"] = nil
			}
			for name, data := range files {
				if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			prelude := `sudo() {
 local status=0
 "$@" || status=$?
 if [ "$STATE" = interrupted-during-read ] && [ "$1" = cat ] && [ "$2" = "$MANIFEST_DIR/components.json" ]; then
  touch "$MANIFEST_DIR/components.updating"
 fi
 return "$status"
}
`
			cmd := exec.Command("bash", "-c", prelude+strings.ReplaceAll(guestConnectionSnapshot, "/etc/claude-cage", dir))
			cmd.Env = append(os.Environ(), "HOME="+dir, "MANIFEST_DIR="+dir, "STATE="+state)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("snapshot: %v\n%s", err, out)
			}
			_, current, err := parseGuestConnection(out)
			switch state {
			case "current":
				if err != nil || current == nil || !current.Current() {
					t.Fatalf("current guest: %v, %v", current, err)
				}
			case "outdated":
				if !errors.Is(err, errGuestUpdateRequired) {
					t.Fatal("outdated guest did not require an update", err)
				}
			default:
				if !errors.Is(err, errGuestRecoveryRequired) || !guestComponentsUnavailable(err) {
					t.Fatal("interrupted/missing manifest did not enter recovery", err)
				}
			}
		})
	}
}

func grantedToolUpdateServer(t *testing.T) (*Server, *cage.EgressGate) {
	t.Helper()
	s, gate := interfaceMonitorServer(t)
	grantTestGate(t, gate)
	s.ready = true
	required := cage.RequiredGuestManifest()
	s.guestManifest = &required
	s.setJobForTest(Job{ID: "tools", Kind: "environment-tools", Running: true})
	s.publishGate(gate.Status())
	return s, gate
}

func grantTestGate(t *testing.T, gate *cage.EgressGate) {
	t.Helper()
	state := gate.Status()
	report := cage.SafetyReport{Policy: cage.SafetyPolicyVersion, ID: "tool-update-test", RunID: state.RunID, CheckID: state.CheckID, Configuration: state.Configuration, CreatedAt: time.Now()}
	for _, id := range []string{"egress_policy", "proxy_transport", "dns_transport", "guest_settings", "browser_settings", "filesystem_boundary", "management_boundary", "fail_closed", "udp_policy", "ipv6_policy", "exit_consistency"} {
		report.Checks = append(report.Checks, cage.SafetyCheck{ID: id, Title: id, Critical: true, State: cage.CheckPassed, Evidence: "synthetic isolated fixture", CheckedAt: report.CreatedAt})
	}
	if err := gate.Grant(report); err != nil {
		t.Fatal(err)
	}
}

func TestToolUpdateLeaseHonorsPauseStopAndConfigurationAtRenewal(t *testing.T) {
	for _, jobKind := range []string{"environment-tools", "claude-desktop-install"} {
		t.Run(jobKind, func(t *testing.T) {
			for _, condition := range []string{"allowed", "paused", "stop", "configuration-changed"} {
				t.Run(condition, func(t *testing.T) {
					s, gate := grantedToolUpdateServer(t)
					s.setJobForTest(Job{ID: "tools", Kind: jobKind, Running: true})
					client, err := s.gateClient()
					if err != nil {
						t.Fatal(err)
					}
					before := gate.Status()
					// Windows can grant and renew within the same wall-clock tick.
					// Advance past the grant tick before requiring a later deadline.
					for !time.Now().Add(cage.GateLease).After(before.Until) {
						time.Sleep(time.Millisecond)
					}
					switch condition {
					case "paused":
						if err := s.pauseNetworkNow("test user pause"); err != nil {
							t.Fatal(err)
						}
					case "stop":
						s.setJobForTest(Job{ID: "stop", Kind: "stop", Running: true})
					case "configuration-changed":
						cfg, err := loadConfig(s.configPath)
						if err != nil {
							t.Fatal(err)
						}
						cfg.Timezone = "Etc/UTC"
						if cfg.Fingerprint() == before.Configuration {
							cfg.Timezone = "Asia/Tokyo"
						}
						if err := writePrivate(s.configPath, cfg); err != nil {
							t.Fatal(err)
						}
					}
					s.finishSafetyMonitor(context.Background(), client, before, "192.0.2.1", "")
					after := gate.Status()
					switch condition {
					case "allowed":
						if after.Mode != cage.GateOpen || !after.Until.After(before.Until) {
							t.Fatal("tool update lease was not renewed")
						}
					case "paused", "configuration-changed":
						if after.Mode != cage.GateClosed {
							t.Fatal("denied network was renewed")
						}
					case "stop":
						if after.Until != before.Until {
							t.Fatal("stopping job allowed a lease renewal")
						}
					}
				})
			}
		})
	}
}

func TestToolUpdateDoesNotRestorePausedOrExpiredNetworking(t *testing.T) {
	for _, paused := range []bool{true, false} {
		s, gate := grantedToolUpdateServer(t)
		if paused {
			if err := s.pauseNetworkNow("manual"); err != nil {
				t.Fatal(err)
			}
		} else {
			gate.Close("expired")
		}
		s.mu.Lock()
		allowed := s.toolsNetworkAllowedLocked()
		s.mu.Unlock()
		if !allowed {
			t.Fatal("business network pause blocked maintenance")
		}
		if gate.Status().Mode != cage.GateClosed {
			t.Fatal("tool update restored closed network")
		}
	}
}

func TestEnvironmentVersionRoutesRequireHostSession(t *testing.T) {
	s := testServer(t)
	for _, path := range []string{"/api/environment/versions", "/api/environment/update", "/api/environment/tools"} {
		method := "POST"
		if strings.HasSuffix(path, "versions") {
			method = "GET"
		}
		if w := request(s, method, path, s.origin, "{}", false); w.Code != 401 {
			t.Fatalf("untrusted version action accepted: %d", w.Code)
		}
	}
	w := request(s, "GET", "/api/environment/versions", s.origin, "", true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"stopped"`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestVersionQueryPublishesUpdateOnlyWithoutNewerDecision(t *testing.T) {
	s := testServer(t)
	status := func() (bool, string) {
		var v struct {
			Required bool   `json:"environment_update_required"`
			Error    string `json:"environment_update_error"`
		}
		w := request(s, "GET", "/api/status", s.origin, "", true)
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(w.Code, err)
		}
		return v.Required, v.Error
	}
	begin := func() uint64 {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.guestUpdateGeneration
	}

	// A query that found a stale manifest is published to status.
	s.noteGuestUpdateRequired(begin())
	if required, message := status(); !required || message != errGuestUpdateRequired.Error() {
		t.Fatalf("stale manifest not published: %v %q", required, message)
	}

	// An attach after the query began (here: update finished) wins.
	s.mu.Lock()
	s.setGuestUpdateLocked(false, "")
	s.mu.Unlock()
	stale := begin()
	s.mu.Lock()
	s.setGuestUpdateLocked(true, "环境组件更新尚未完成，请等待结果；失败时可以重试。")
	s.setGuestUpdateLocked(false, "")
	s.mu.Unlock()
	s.noteGuestUpdateRequired(stale)
	if required, _ := status(); required {
		t.Fatal("a query started before the update overwrote its result")
	}

	// Never while a maintenance job runs.
	release := make(chan struct{})
	if _, err := s.jobs.submit(&sync.Mutex{}, "environment-update", 0, nil, nil, func() error { <-release; return nil }); err != nil {
		t.Fatal(err)
	}
	s.noteGuestUpdateRequired(begin())
	close(release)
	if required, _ := status(); required {
		t.Fatal("a query during maintenance published its result")
	}
}

func TestMissingOrTruncatedManifestFailClosesAndRecoversOnReattach(t *testing.T) {
	if _, err := os.Stat(sshExecutable()); err != nil {
		t.Skip("OpenSSH unavailable")
	}
	for _, corrupt := range []string{"null", `{"protocol":`} {
		t.Run("manifest-"+corrupt, func(t *testing.T) {
			s := transferMountServer(t, true, "current")
			cfg, err := loadConfig(s.configPath)
			if err != nil {
				t.Fatal(err)
			}
			gate, err := cage.NewEgressGate(cfg)
			if err != nil {
				t.Fatal(err)
			}
			stopGate, err := cage.StartGateControl(s.configPath, s.instanceDir(), gate)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(stopGate)
			if err := gate.BeginCheck(); err != nil {
				t.Fatal(err)
			}
			grantTestGate(t, gate)
			s.publishGate(gate.Status())

			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			_, key, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			signer, err := ssh.NewSignerFromKey(key)
			if err != nil {
				t.Fatal(err)
			}
			sshConfig := &ssh.ServerConfig{NoClientAuth: true}
			sshConfig.AddHostKey(signer)
			var mu sync.Mutex
			manifestPayload := corrupt
			go func() {
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					go func() {
						server, channels, requests, err := ssh.NewServerConn(conn, sshConfig)
						if err != nil {
							_ = conn.Close()
							return
						}
						go ssh.DiscardRequests(requests)
						for incoming := range channels {
							ch, reqs, err := incoming.Accept()
							if err != nil {
								break
							}
							for req := range reqs {
								if req.Type != "exec" {
									_ = req.Reply(false, nil)
									continue
								}
								_ = req.Reply(true, nil)
								mu.Lock()
								payload := manifestPayload
								mu.Unlock()
								_, _ = ch.Write([]byte("{\"token\":\"guest-token\"}\nvnc-secret\n" + payload + "\n"))
								_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
								break
							}
							_ = ch.Close()
						}
						_ = server.Close()
					}()
				}
			}()
			clientConfig := fmt.Sprintf("Host *\n HostName 127.0.0.1\n Port %d\n User cage\n StrictHostKeyChecking no\n UserKnownHostsFile %s\n", listener.Addr().(*net.TCPAddr).Port, os.DevNull)
			if err := os.WriteFile(filepath.Join(s.instanceDir(), "ssh.config"), []byte(clientConfig), 0600); err != nil {
				t.Fatal(err)
			}

			// First attach sees missing/truncated manifest from interrupted boot:
			// must fail-closed (close proxy gate) and flag environment_update_required.
			if err := s.attach(); !guestComponentsUnavailable(err) {
				t.Fatalf("attach with %q: got %v, want components unavailable", corrupt, err)
			}
			if gate.Status().Mode != cage.GateClosed {
				t.Fatalf("proxy gate stayed open despite %q manifest", corrupt)
			}
			s.mu.Lock()
			needsUpdate := s.guestUpdateRequired
			ready := s.ready
			s.mu.Unlock()
			if !needsUpdate || ready {
				t.Fatalf("state after %q manifest: needsUpdate=%v ready=%v", corrupt, needsUpdate, ready)
			}

			// Once retry republishes the current manifest, parseGuestConnection accepts it.
			required, _ := json.Marshal(cage.RequiredGuestManifest())
			mu.Lock()
			manifestPayload = string(required)
			mu.Unlock()
			creds, parsed, err := parseGuestConnection([]byte("{\"token\":\"guest-token\"}\nvnc-secret\n" + string(required)))
			if err != nil || parsed == nil || !parsed.Current() || creds.Token != "guest-token" {
				t.Fatalf("republished manifest not accepted: creds=%+v parsed=%+v err=%v", creds, parsed, err)
			}
		})
	}
}
