package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestDesktopLocalProbeLeavesColdProfileUntouched(t *testing.T) {
	root := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	d := NewDesktopLocalController(filepath.Join(root, "local", "config.json"), exe)
	st, err := d.Status(context.Background())
	if err != nil || !st.Available || !st.Known || st.Running || st.ServiceRunning {
		t.Fatalf("cold probe: %+v %v", st, err)
	}
	if _, err = d.Attach(context.Background(), "departed-controller"); !errors.Is(err, errControllerUnavailable) {
		t.Fatalf("window-only attach to a stopped service: %v", err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("probe initialized a local workload: %v %v", entries, err)
	}
}

func TestDesktopLocalReleaseHasIndependentLauncherAndAccountGuards(t *testing.T) {
	s := testServer(t)
	path := filepath.Join(s.profile, "state", "lima", "fixture", "cage-running.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(path, map[string]int{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	s.account.lock()
	observed := s.desktopLocalSnapshot().ControllerID
	for _, tc := range []struct {
		name, token, identity string
		stop                  bool
		status                int
	}{
		{"browser cookie cannot stop", "", observed, true, 401},
		{"previous controller", s.token, "previous", true, 409},
		{"idle handoff cannot stop workload", s.token, observed, false, 409},
		{"locked account", s.token, observed, true, 423},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(desktopLocalRequest{Token: tc.token, ControllerID: tc.identity, Running: true, Stop: tc.stop})
			got := request(s, "POST", "/api/desktop-local/release", s.origin, string(b), true)
			if got.Code != tc.status || s.jobs.snapshot().ID != "" {
				t.Fatalf("result=%d, job=%+v", got.Code, s.jobs.snapshot())
			}
		})
	}
	s.setJobForTest(Job{ID: "another-completed-job"})
	b, _ := json.Marshal(desktopLocalRequest{Token: s.token, ControllerID: observed, Running: true, Stop: true})
	if w := request(s, "POST", "/api/desktop-local/release", s.origin, string(b), true); w.Code != http.StatusConflict {
		t.Fatalf("old workload confirmation was accepted: %d", w.Code)
	}
}

func TestDesktopLocalStopMustFinishItsOwnJobBeforeHandoff(t *testing.T) {
	s := testServer(t)
	s.terminals.sessions = map[string]*terminalSession{"live": {id: "live"}}
	exited := make(chan struct{})
	s.shutdown = func() { close(exited) }
	release := func(input desktopLocalRequest) (int, desktopLocalReply) {
		input.Token = s.token
		b, _ := json.Marshal(input)
		w := request(s, "POST", "/api/desktop-local/release", s.origin, string(b), false)
		var reply desktopLocalReply
		if w.Code == 200 {
			if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
				t.Fatal(err)
			}
		}
		return w.Code, reply
	}
	view := s.desktopLocalSnapshot()
	code, job := release(desktopLocalRequest{ControllerID: view.ControllerID, Terminals: 1, Stop: true})
	if code != 200 || !job.JobRunning || job.JobID == "" {
		t.Fatalf("stop %d %+v", code, job)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		view = s.desktopLocalSnapshot()
		if !view.JobRunning || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if view.JobID != job.JobID || view.JobFailed || view.JobRunning || view.LiveTerminals != 0 {
		t.Fatalf("job outcome %+v", view)
	}
	if code, _ := release(desktopLocalRequest{ControllerID: view.ControllerID, JobID: "different"}); code != 409 {
		t.Fatalf("handoff for different job %d", code)
	}
	if code, _ := release(desktopLocalRequest{ControllerID: view.ControllerID, JobID: job.JobID}); code != 200 {
		t.Fatalf("handoff %d", code)
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("approved idle owner did not exit")
	}
}

// A real full controller owns the lock and launch ticket. This test never
// boots a VM. It verifies that closing a view preserves its controller and
// that only its currently reported identity can release the idle process.
func TestDesktopLocalAuthenticatedViewAndIdleExit(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "local")
	config := filepath.Join(profile, "config.json")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestServeShutdownHandoff$", "-test.timeout=25s")
	cmd.Env = append(os.Environ(), "CAGE_TEST_SHUTDOWN_CONFIG="+config)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})
	d := NewDesktopLocalController(config, exe)
	var st DesktopLocalStatus
	for {
		st, err = d.Status(ctx)
		if err == nil && st.Known && st.ServiceRunning {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("controller did not start")
		case <-done:
			t.Fatal("controller exited", waitErr)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if _, err := d.Attach(ctx, "previous-controller"); err == nil {
		t.Fatal("window-only attach accepted a replaced controller")
	}
	attached, err := d.Attach(ctx, st.ControllerID)
	if err != nil {
		t.Fatal(err)
	}
	_ = attached.Close()
	session, err := d.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	req, _ := http.NewRequestWithContext(ctx, "GET", session.Origin+"/api/account", nil)
	req.Header.Set("Cookie", "untrusted=123")
	req.Header.Set("Origin", "https://not-the-controller.invalid")
	res, err := session.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("authenticated view: %d", res.StatusCode)
	}
	if err = session.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := d.Status(ctx)
	if err != nil || !after.ServiceRunning || after.ControllerID != st.ControllerID {
		t.Fatalf("view closed controller: %+v %v", after, err)
	}
	if released, err := d.(*desktopLocal).ReleaseOwnedIdle(ctx, after); err != nil || released {
		t.Fatalf("independently started service auto-release: released=%v err=%v", released, err)
	}
	if after, err = d.Status(ctx); err != nil || !after.ServiceRunning || after.ControllerID != st.ControllerID {
		t.Fatalf("independent owner was lost: %+v %v", after, err)
	}
	stale := st
	stale.ControllerID = fmt.Sprintf("%064x", 1)
	if err = d.Release(ctx, stale, false); err == nil {
		t.Fatal("stale handoff accepted")
	}
	if err = d.Release(ctx, st, false); err != nil {
		t.Fatal(err)
	}
	<-done
	if waitErr != nil {
		t.Fatal(waitErr)
	}
	if _, err := os.Stat(filepath.Join(profile, "control.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("released controller record still exists: %v", err)
	}
}

func TestDesktopLocalReleaseIdleStandaloneRestartsOnlyNonDesktopIdleController(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		launchID  string
		wantAdopt bool
	}{
		{"standalone-empty-launch-id", "", true},
		{"desktop-owned-launch-id", "desktop-launch-123", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := filepath.Join(t.TempDir(), "local")
			config := filepath.Join(profile, "config.json")
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestServeShutdownHandoff$", "-test.timeout=25s")
			env := append(os.Environ(), "CAGE_TEST_SHUTDOWN_CONFIG="+config)
			if tc.launchID != "" {
				env = append(env, "CAGE_DESKTOP_START_ID="+tc.launchID)
			}
			cmd.Env = env
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			var waitErr error
			go func() { waitErr = cmd.Wait(); close(done) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
				}
			})
			d := NewDesktopLocalController(config, exe).(*desktopLocal)
			d.adoptIdleStandalone = true
			var st DesktopLocalStatus
			for {
				st, err = d.Status(ctx)
				if err == nil && st.Known && st.ServiceRunning {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("controller did not start")
				case <-done:
					t.Fatal("controller exited", waitErr)
				case <-time.After(50 * time.Millisecond):
				}
			}
			adopted, err := d.releaseIdleStandalone(ctx, st)
			if err != nil || adopted != tc.wantAdopt {
				t.Fatalf("releaseIdleStandalone = (%v, %v), want (%v, nil)", adopted, err, tc.wantAdopt)
			}
			if tc.wantAdopt {
				<-done
				if waitErr != nil {
					t.Fatal(waitErr)
				}
				if _, err := os.Stat(filepath.Join(profile, "control.json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("released standalone record still exists: %v", err)
				}
			} else {
				after, err := d.Status(ctx)
				if err != nil || !after.ServiceRunning || after.ControllerID != st.ControllerID {
					t.Fatalf("desktop-launched controller was unexpectedly released: %+v %v", after, err)
				}
				if err := d.Release(ctx, after, false); err != nil {
					t.Fatal(err)
				}
				<-done
			}
		})
	}
}

func TestDesktopLocalPreserveVMReleaseGuards(t *testing.T) {
	s := testServer(t)
	marker := filepath.Join(s.profile, "state", "lima", "fixture", "cage-running.json")
	if err := os.MkdirAll(filepath.Dir(marker), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(marker, map[string]int{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	s.shutdown = func() { close(exited) }
	s.account.lock()

	// Stop + PreserveVM together must be rejected.
	view := s.desktopLocalSnapshot()
	b, _ := json.Marshal(desktopLocalRequest{Token: s.token, ControllerID: view.ControllerID, Running: true, Stop: true, PreserveVM: true})
	if w := request(s, "POST", "/api/desktop-local/release", s.origin, string(b), false); w.Code != http.StatusConflict {
		t.Fatalf("stop+preserve_vm returned %d, want 409", w.Code)
	}

	// Live terminals must refuse PreserveVM handoff.
	s.terminals.sessions = map[string]*terminalSession{"live": {id: "live"}}
	view = s.desktopLocalSnapshot()
	b, _ = json.Marshal(desktopLocalRequest{Token: s.token, ControllerID: view.ControllerID, Running: true, Terminals: 1, PreserveVM: true})
	if w := request(s, "POST", "/api/desktop-local/release", s.origin, string(b), false); w.Code != http.StatusConflict {
		t.Fatalf("preserve_vm with live terminal returned %d, want 409", w.Code)
	}
	s.terminals.sessions = nil

	// Running VM with no live terminals and no active job may hand off even while locked.
	view = s.desktopLocalSnapshot()
	b, _ = json.Marshal(desktopLocalRequest{Token: s.token, ControllerID: view.ControllerID, Running: true, PreserveVM: true})
	if w := request(s, "POST", "/api/desktop-local/release", s.origin, string(b), false); w.Code != http.StatusOK {
		t.Fatalf("preserve_vm returned %d %s, want 200", w.Code, w.Body.String())
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("preserve_vm handoff did not trigger controller shutdown")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("running VM marker was disturbed: %v", err)
	}
}

func TestDesktopLocalReleaseStaleSessionPreservesRunningVM(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(t.TempDir(), "local")
	config := filepath.Join(profile, "config.json")
	marker := filepath.Join(profile, "state", "lima", "fixture", "cage-running.json")
	if err := os.MkdirAll(filepath.Dir(marker), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(marker, map[string]int{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestServeShutdownHandoff$", "-test.timeout=25s")
	cmd.Env = append(os.Environ(),
		"CAGE_TEST_SHUTDOWN_CONFIG="+config,
		"CAGE_DESKTOP_START_ID=desktop-launch-old",
		"CAGE_TEST_DESKTOP_SESSION=darwin-gui:100002",
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})

	d := NewDesktopLocalController(config, exe).(*desktopLocal)
	var st DesktopLocalStatus
	for {
		st, err = d.Status(ctx)
		if err == nil && st.Known && st.ServiceRunning && st.Running {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("controller did not start with running VM: %+v %v", st, err)
		case <-done:
			t.Fatalf("controller exited early: %v", waitErr)
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Same desktop session must keep the running controller untouched.
	replaced, err := d.releaseStaleSession(ctx, st, "darwin-gui:100002")
	if err != nil || replaced {
		t.Fatalf("same session releaseStaleSession = (%v, %v), want (false, nil)", replaced, err)
	}

	// Different desktop session must hand off the controller while preserving the VM.
	replaced, err = d.releaseStaleSession(ctx, st, "darwin-gui:111303")
	if err != nil || !replaced {
		t.Fatalf("stale session releaseStaleSession = (%v, %v), want (true, nil)", replaced, err)
	}
	<-done
	if waitErr != nil {
		t.Fatalf("stale controller did not exit cleanly: %v", waitErr)
	}
	if _, err := os.Stat(filepath.Join(profile, "control.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale controller record still exists: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("running VM marker was removed: %v", err)
	}
}

func TestSessionChangedHelper(t *testing.T) {
	orig := desktopSessionID
	t.Cleanup(func() { desktopSessionID = orig })

	desktopSessionID = func() string { return "darwin-gui:111303" }
	if sessionChanged("") {
		t.Fatal("empty controller session should not trigger handoff")
	}
	if sessionChanged("darwin-gui:111303") {
		t.Fatal("matching session should not trigger handoff")
	}
	if !sessionChanged("darwin-gui:100002") {
		t.Fatal("mismatched session should trigger handoff")
	}
	desktopSessionID = func() string { return "" }
	if sessionChanged("darwin-gui:100002") {
		t.Fatal("empty current session should not trigger handoff")
	}
}
