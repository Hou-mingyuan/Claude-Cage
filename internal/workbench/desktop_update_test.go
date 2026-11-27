package workbench

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type updateStopTransport func(*http.Request) (*http.Response, error)

func (f updateStopTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUpdateHandoffWaitsForSlowStop(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stopAfter  time.Duration
		wantUpdate bool
	}{
		{name: "Lima stop finishes after the old updater deadline", stopAfter: 6*time.Minute + 10*time.Second, wantUpdate: true},
		{name: "stop never completes within the update deadline", stopAfter: 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := t.TempDir()
			lock, err := openControllerLock(profile, os.O_CREATE|os.O_RDWR)
			if err != nil {
				t.Fatal(err)
			}
			if err := lock.Close(); err != nil {
				t.Fatal(err)
			}
			synctest.Test(t, func(t *testing.T) {
				var stopRequested time.Time
				handedOff := false
				client := &controllerClient{
					profile: profile,
					origin:  "http://localhost:17778",
					launch:  launchResponse{URL: "http://localhost:17778/#ticket", Controller: ControllerIdentity{Protocol: controlProtocol}},
					client: &http.Client{Transport: updateStopTransport(func(req *http.Request) (*http.Response, error) {
						var body string
						switch req.URL.Path {
						case "/api/session", "/api/client/context":
							body = `{}`
						case "/api/stop":
							stopRequested = time.Now()
							body = `{"id":"stop-job","running":true}`
						case "/api/status":
							if time.Since(stopRequested) < tc.stopAfter {
								body = `{"vm_running":true,"job":{"id":"stop-job","running":true}}`
							} else {
								body = `{"vm_running":false,"job":{"id":"stop-job","running":false}}`
							}
						case "/api/terminals":
							body = `[]`
						case "/api/handoff":
							if time.Since(stopRequested) < tc.stopAfter {
								t.Error("handoff preceded completion of the stop job")
							}
							handedOff = true
							body = `{}`
						default:
							t.Fatalf("unexpected request %s", req.URL.Path)
						}
						return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
					})},
				}
				err := client.stopAndHandoff()
				if tc.wantUpdate && (err != nil || !handedOff) {
					t.Fatalf("stopped after %v, handoff=%t, error=%v", tc.stopAfter, handedOff, err)
				}
				if !tc.wantUpdate && (handedOff || err == nil || !strings.Contains(err.Error(), "停止环境超时")) {
					t.Fatalf("unfinished stop handed off=%t, error=%v", handedOff, err)
				}
			})
		})
	}
}

func TestUpdaterTrustedStop(t *testing.T) {
	for _, tc := range []struct {
		name         string
		configure    func(*controllerClient, *Server)
		wantProof    bool
		stopStatus   int
		wantOK       bool
		switchRemote bool
		replaceJob   bool
	}{
		{name: "trusted unlocked", wantProof: true, stopStatus: http.StatusOK, wantOK: true},
		{name: "changed stop job", replaceJob: true, wantProof: true, stopStatus: http.StatusOK},
		{name: "switch to remote after context", switchRemote: true, wantProof: true, stopStatus: http.StatusConflict},
		{name: "password unlocked", configure: func(_ *controllerClient, s *Server) {
			s.account.mu.Lock()
			s.account.trustedSession = false
			s.account.mu.Unlock()
		}, wantProof: true, stopStatus: http.StatusOK, wantOK: true},
		{name: "locked", configure: func(_ *controllerClient, s *Server) {
			s.account.lock()
		}, wantProof: true, stopStatus: http.StatusLocked},
		{name: "different profile", configure: func(c *controllerClient, _ *Server) {
			c.launch.ConfigPath = filepath.Join(c.profile, "other", "config.json")
		}, stopStatus: http.StatusLocked},
		{name: "stale generation proof", configure: func(c *controllerClient, _ *Server) {
			c.record.Token = randomToken()
		}, wantProof: true, stopStatus: http.StatusLocked},
		{name: "gateway", configure: func(c *controllerClient, _ *Server) {
			c.launch.DesktopGateway = true
		}, stopStatus: http.StatusLocked},
		{name: "client only", configure: func(c *controllerClient, _ *Server) {
			c.launch.ClientOnly = true
		}, stopStatus: http.StatusLocked},
		{name: "legacy", configure: func(c *controllerClient, _ *Server) {
			c.launch.Controller.Protocol = 0
		}},
		{name: "bad ticket", configure: func(c *controllerClient, _ *Server) {
			u, _ := url.Parse(c.launch.URL)
			u.Fragment = "invalid"
			c.launch.URL = u.String()
		}},
		{name: "expired ticket", configure: func(c *controllerClient, s *Server) {
			u, _ := url.Parse(c.launch.URL)
			s.mu.Lock()
			s.tickets[u.Fragment] = time.Now().Add(-time.Second)
			s.mu.Unlock()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(t)
			s.account.trustedSession = true
			s.remote = newRemoteGateway(s.profile)
			t.Cleanup(s.remote.close)
			s.identity = ControllerIdentity{Protocol: controlProtocol, PID: os.Getpid(), Build: "previous-version"}

			lock, err := openControllerLock(s.profile, os.O_CREATE|os.O_RDWR)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err := lockController(lock); err != nil {
				t.Fatal(err)
			}
			defer unlockController(lock)

			var requestMu sync.Mutex
			var stopStatus, statusReads int
			var stopProof, handedOff bool
			handler := s.handler()
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestMu.Lock()
				defer requestMu.Unlock()
				switch r.URL.Path {
				case "/api/client/context":
					handler.ServeHTTP(w, r)
					if tc.switchRemote {
						s.remote.setRemote(true)
					}
				case "/api/stop", "/api/status":
					// Apply the real HTTP account and session guards without
					// allowing this fixture to launch or stop a guest process.
					check := httptest.NewRecorder()
					guarded := r.Clone(r.Context())
					guarded.Method = http.MethodGet
					guarded.URL.Path = "/api/terminals"
					handler.ServeHTTP(check, guarded)
					if r.URL.Path == "/api/stop" {
						stopStatus = check.Code
						_, err := r.Cookie(s.cookieName() + "-desktop")
						stopProof = err == nil
						if tc.switchRemote && requestConnection(r) != "local" {
							t.Error("updater stop did not pin the local connection")
						}
					}
					if check.Code != http.StatusOK {
						w.WriteHeader(check.Code)
						return
					}
					job := Job{ID: "stop-job", Running: true}
					if r.URL.Path == "/api/stop" {
						jsonReply(w, job)
						return
					}
					statusReads++
					job.Running = statusReads == 1
					if tc.replaceJob {
						job.ID = "replacement-job"
					}
					jsonReply(w, map[string]any{"job": job, "vm_running": job.Running})
				case "/api/handoff":
					if !s.launcherAuthenticated(w, r) {
						return
					}
					handedOff = true
					if statusReads != 2 {
						t.Error("handoff preceded completion of the stop job")
					}
					if err := unlockController(lock); err != nil {
						t.Error("release fixture controller lock:", err)
					}
					jsonReply(w, struct{}{})
				default:
					handler.ServeHTTP(w, r)
				}
			}))
			defer server.Close()
			port := server.Listener.Addr().(*net.TCPAddr).Port
			s.origin = "http://localhost:" + strconv.Itoa(port)
			server.Start()
			record := controlFile{Port: port, Token: s.token, PID: os.Getpid(), Controller: s.identity}
			if err := writePrivate(filepath.Join(s.profile, "control.json"), record); err != nil {
				t.Fatal(err)
			}
			c, err := connectController(s.profile)
			if err != nil {
				t.Fatal(err)
			}
			defer c.close()
			if tc.configure != nil {
				tc.configure(c, s)
			}
			err = c.stopAndHandoff()
			if tc.stopStatus == 0 {
				origin, _ := url.Parse(c.origin)
				for _, cookie := range c.client.Jar.Cookies(origin) {
					if cookie.Name == s.cookieName()+"-desktop" {
						t.Error("updater minted proof without completing the session")
					}
				}
			}
			server.Close()
			requestMu.Lock()
			defer requestMu.Unlock()
			if (err == nil) != tc.wantOK || handedOff != tc.wantOK || stopStatus != tc.stopStatus || stopProof != tc.wantProof {
				t.Fatalf("err=%v handoff=%t stop HTTP=%d proof present=%t", err, handedOff, stopStatus, stopProof)
			}
		})
	}
}

func TestUpdaterProofWithoutSession(t *testing.T) {
	s := testServer(t)
	s.account.trustedSession = true
	r := httptest.NewRequest(http.MethodPost, s.origin+"/api/stop", http.NoBody)
	r.Header.Set("Origin", s.origin)
	r.AddCookie(&http.Cookie{Name: s.cookieName() + "-desktop", Value: trustedViewProof(s.token)})
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || s.jobs.snapshot().ID != "" {
		t.Fatal("desktop proof bypassed ordinary session validation")
	}
}
