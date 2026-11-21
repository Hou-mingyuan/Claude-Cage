package workbench

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/guestos"
	"github.com/sylvinus/agent-vm/internal/guestprofile"
	"github.com/sylvinus/agent-vm/internal/hostimage"
)

func TestAbandonedGuestsReportLeftoverDataWithoutTouchingIt(t *testing.T) {
	s := testServer(t)
	status := func() []string {
		t.Helper()
		response := desktopRequestForTest(s, http.MethodGet, "/api/status", nil)
		var view struct {
			Abandoned []string `json:"abandoned_guests"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &view) != nil || view.Abandoned == nil {
			t.Fatalf("status lacks the abandoned guest list: %d %s", response.Code, response.Body.String())
		}
		return view.Abandoned
	}
	if found := status(); len(found) != 0 {
		t.Fatalf("clean profile reported abandoned guests: %v", found)
	}
	const content = `{"version":1}`
	for _, leftover := range []struct {
		system, path string
		directory    bool
	}{
		{"windows", "windows-guest.json", false},
		{"windows", filepath.Join("state", "windows-0123456789abcdef0123456789abcdef"), true},
		{"macos", "macos-guest.json", false},
		{"macos", filepath.Join("state", "lima", "mac"), true},
		{"macos", filepath.Join("state", "guest-media", "macos"), true},
		{"ubuntu", filepath.Join("state", "guest-media", "ubuntu-22.04"), true},
		{"ubuntu", filepath.Join("state", "guest-media", "ubuntu-24.04"), true},
	} {
		path := filepath.Join(s.profile, leftover.path)
		var err error
		if leftover.directory {
			err = os.MkdirAll(path, 0700)
		} else {
			err = os.WriteFile(path, []byte(content), 0600)
		}
		if err != nil {
			t.Fatal(err)
		}
		if found := abandonedGuests(s.profile); !slices.Equal(found, []string{leftover.system}) {
			t.Fatalf("%s not reported as %s: %v", leftover.path, leftover.system, found)
		}
		info, err := os.Lstat(path)
		if err != nil || info.IsDir() != leftover.directory {
			t.Fatal("reporting changed the leftover entry", leftover.path, err)
		}
		if !leftover.directory {
			if data, err := os.ReadFile(path); err != nil || string(data) != content {
				t.Fatal("reporting changed the leftover file", leftover.path, err)
			}
		}
		// Each case stands alone; the test owns this temporary profile.
		if err = os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"windows-guest.json", "macos-guest.json"} {
		if err := os.WriteFile(filepath.Join(s.profile, path), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(s.profile, "state", "guest-media", "ubuntu-22.04"), 0700); err != nil {
		t.Fatal(err)
	}
	if found := status(); !slices.Equal(found, []string{"macos", "ubuntu", "windows"}) {
		t.Fatalf("status did not list every abandoned guest: %v", found)
	}
}

func TestAbandonedInstallationRecordIsReportedAndRefused(t *testing.T) {
	s := testServer(t)
	install := guestos.Installation{Version: 1, System: guestos.MacOS, ID: uuid.New()}
	if err := guestos.SaveInstallation(s.profile, install); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(s.profile, "guest-install.json")
	before, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if found := s.abandonedGuestList(); !slices.Equal(found, []string{"macos"}) {
		t.Fatalf("pending macOS installation not reported: %v", found)
	}
	for _, body := range []string{`{}`, `{"guest_system":"macos"}`, `{"guest_system":"windows-11"}`} {
		response := desktopRequestForTest(s, http.MethodPost, "/api/start", []byte(body))
		if response.Code != http.StatusConflict {
			t.Fatalf("abandoned installation was started by %s: %d %s", body, response.Code, response.Body.String())
		}
	}
	if s.jobs.snapshot().Running {
		t.Fatal("refused start still launched a job")
	}
	view, err := s.guestInstallView()
	if err != nil || view.Available || view.Reason == "" || view.Pending == nil {
		t.Fatalf("install view does not explain the abandoned record: %+v %v", view, err)
	}
	if after, err := os.ReadFile(record); err != nil || string(after) != string(before) {
		t.Fatal("refusal changed the installation record", err)
	}
}

func ubuntuPlanForTest(profile, system string) hostimage.Plan {
	version := strings.TrimPrefix(system, "ubuntu-")
	return hostimage.Plan{
		Host:    hostimage.Identity{System: system, Version: version, Arch: "amd64"},
		Version: version, Format: "qcow2", Path: filepath.Join(profile, "state", "guest-media", system, "image.qcow2"),
		Digest: "sha256:" + strings.Repeat("a", 64),
	}
}

// profileFilesForTest snapshots every regular file below the profile.
func profileFilesForTest(t *testing.T, profile string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(profile, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		files[path] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// An environment that an earlier version created from Ubuntu is reported, is
// never started or reinstalled as Debian, and keeps every file.
func TestExistingUbuntuEnvironmentIsReportedRefusedAndUntouched(t *testing.T) {
	for _, system := range []string{"ubuntu-22.04", "ubuntu-24.04"} {
		t.Run(system, func(t *testing.T) {
			s := testServer(t)
			media := filepath.Join(s.profile, "state", "guest-media")
			plan := ubuntuPlanForTest(s.profile, system)
			if err := hostimage.Save(media, plan); err != nil {
				t.Fatal(err)
			}
			// The durable image choice and the disk of the finished environment.
			if err := os.WriteFile(filepath.Join(media, "host-system.yaml"), []byte(`{"images":[]}`), 0600); err != nil {
				t.Fatal(err)
			}
			disk := filepath.Join(s.profile, "state", "lima", "base")
			if err := os.MkdirAll(disk, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(disk, "diffdisk"), []byte("user data"), 0600); err != nil {
				t.Fatal(err)
			}
			before := profileFilesForTest(t, s.profile)

			if found := s.abandonedGuestList(); !slices.Equal(found, []string{"ubuntu"}) {
				t.Fatalf("existing %s environment not reported: %v", system, found)
			}
			response := desktopRequestForTest(s, http.MethodGet, "/api/status", nil)
			var view struct {
				Abandoned []string `json:"abandoned_guests"`
				System    string   `json:"guest_system"`
				Error     string   `json:"guest_system_error"`
			}
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &view) != nil ||
				!slices.Equal(view.Abandoned, []string{"ubuntu"}) || view.System != "" || !strings.Contains(view.Error, "Ubuntu") {
				t.Fatalf("status does not explain the Ubuntu environment: %d %s", response.Code, response.Body.String())
			}
			for _, body := range []string{`{}`, `{"guest_system":"debian-13"}`, `{"guest_system":"` + system + `"}`} {
				response := desktopRequestForTest(s, http.MethodPost, "/api/start", []byte(body))
				if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "Ubuntu") {
					t.Fatalf("%s environment was started by %s: %d %s", system, body, response.Code, response.Body.String())
				}
			}
			if s.jobs.snapshot().Running || s.jobs.snapshot().ID != "" {
				t.Fatal("refused start still launched a job")
			}
			if err := s.startEnvironment(false); err == nil || !strings.Contains(err.Error(), "Ubuntu") {
				t.Fatalf("indirect start of the Ubuntu environment: %v", err)
			}
			after := profileFilesForTest(t, s.profile)
			for path, content := range before {
				if got, ok := after[path]; !ok || got != content {
					t.Fatal("refusing the Ubuntu environment changed", path)
				}
			}
		})
	}
}

// A pending Ubuntu installation is explained and refused. It can only be
// withdrawn while nothing was created for it, exactly like the other
// abandoned guests.
func TestPendingUbuntuInstallationIsRefusedAndOnlyWithdrawnUnstarted(t *testing.T) {
	for name, kind := range map[string]string{"catalog": guestos.LinuxImageSource, "host": ""} {
		t.Run(name, func(t *testing.T) {
			s := testServer(t)
			install := guestos.Installation{Version: 1, ID: uuid.New(), System: guestos.Ubuntu2204,
				Source: ubuntuPlanForTest(s.profile, guestos.Ubuntu2204), SourceKind: kind}
			if err := guestos.SaveInstallation(s.profile, install); err != nil {
				t.Fatal(err)
			}
			record := filepath.Join(s.profile, "guest-install.json")
			before, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			if found := s.abandonedGuestList(); !slices.Equal(found, []string{"ubuntu"}) {
				t.Fatalf("pending Ubuntu installation not reported: %v", found)
			}
			for _, body := range []string{`{}`, `{"guest_system":"ubuntu-22.04"}`, `{"guest_system":"debian-13"}`} {
				response := desktopRequestForTest(s, http.MethodPost, "/api/start", []byte(body))
				if response.Code != http.StatusConflict {
					t.Fatalf("pending Ubuntu installation was started by %s: %d %s", body, response.Code, response.Body.String())
				}
			}
			if s.jobs.snapshot().Running {
				t.Fatal("refused start still launched a job")
			}
			if err := s.startEnvironment(false); err == nil {
				t.Fatal("pending Ubuntu installation started indirectly")
			}
			view, err := s.guestInstallView()
			if err != nil || view.Available || !view.Reselect || !strings.Contains(view.Reason, "Ubuntu") || view.Pending == nil {
				t.Fatalf("install view does not explain the Ubuntu record: %+v %v", view, err)
			}
			// Once a disk exists the record stays, with the disk.
			disk := filepath.Join(s.profile, "state", "lima", "base-building")
			if err := os.MkdirAll(disk, 0700); err != nil {
				t.Fatal(err)
			}
			withdraw := []byte(`{"id":"` + install.ID.String() + `"}`)
			if response := desktopRequestForTest(s, http.MethodDelete, "/api/environment/guest-systems", withdraw); response.Code != http.StatusConflict {
				t.Fatalf("withdrew a started Ubuntu installation: %d %s", response.Code, response.Body.String())
			}
			if after, err := os.ReadFile(record); err != nil || string(after) != string(before) {
				t.Fatal("refusal changed the installation record", err)
			}
			if _, err := os.Stat(disk); err != nil {
				t.Fatal("refusal affected the disk", err)
			}
			if err := os.Remove(disk); err != nil {
				t.Fatal(err)
			}
			if response := desktopRequestForTest(s, http.MethodDelete, "/api/environment/guest-systems", withdraw); response.Code != http.StatusOK {
				t.Fatalf("unstarted Ubuntu installation could not be withdrawn: %d %s", response.Code, response.Body.String())
			}
			if pending, err := guestos.LoadInstallation(s.profile); err != nil || pending != nil {
				t.Fatal("withdrawn record is still pending", err)
			}
		})
	}
}

// Nothing offers or accepts a new system choice: a profile without an
// environment starts Debian 13, and every other system is refused.
func TestNewEnvironmentAcceptsOnlyDebian13(t *testing.T) {
	s := testServer(t)
	view, err := s.guestInstallView()
	if err != nil || view.Pending != nil || view.Available || view.Reason != "" {
		t.Fatalf("clean profile has an installation view: %+v %v", view, err)
	}
	for _, body := range []string{`{"guest_system":"ubuntu-22.04"}`, `{"guest_system":"ubuntu-24.04"}`, `{"guest_system":"debian-12"}`} {
		response := desktopRequestForTest(s, http.MethodPost, "/api/start", []byte(body))
		if response.Code != http.StatusConflict || s.jobs.snapshot().ID != "" {
			t.Fatalf("%s was accepted: %d %s", body, response.Code, response.Body.String())
		}
	}
	if response := desktopRequestForTest(s, http.MethodGet, "/api/environment/guest-systems/clone", nil); response.Code == http.StatusOK {
		t.Fatalf("host matching is still served: %d", response.Code)
	}
	if _, err := os.Stat(filepath.Join(s.profile, "guest-install.json")); !os.IsNotExist(err) {
		t.Fatal("a refused choice left an installation record", err)
	}
}

// ubuntuEnvironmentForTest lays out what an earlier version left of an
// environment created from Ubuntu, beside files that belong to no guest.
// It returns the paths, relative to state, of the environment's own files.
func ubuntuEnvironmentForTest(t *testing.T, s *Server, system string) []string {
	t.Helper()
	stateDir := filepath.Join(s.profile, "state")
	if err := hostimage.Save(filepath.Join(stateDir, "guest-media"), ubuntuPlanForTest(s.profile, system)); err != nil {
		t.Fatal(err)
	}
	instance := cage.InstanceName(s.profile, s.environmentDir())
	owned := []string{
		filepath.Join("guest-media", "host-system.yaml"),
		filepath.Join("guest-media", system, "image.qcow2"),
		filepath.Join("lima", "base", "diffdisk"),
		filepath.Join("lima", instance, "diffdisk"),
		filepath.Join("lima", instance, "lima.yaml"),
		".agent-vm-base-version",
		".agent-vm-base-built-by",
		".agent-vm-version-" + instance,
		".agent-vm-built-by-" + instance,
		filepath.Join("file-mounts", instance, "gitconfig"),
	}
	for _, rel := range append(owned, filepath.Join("lima", "_config", "user"), filepath.Join("guest-media", "debian-13.txt"), "device-reset-last.json") {
		path := filepath.Join(stateDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("content of "+rel), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := guestprofile.Create(s.profile, guestprofile.Options{}); err != nil {
		t.Fatal(err)
	}
	return append(owned, filepath.Join("guest-media", "host-system-source.json"))
}

func removedGuestFoldersForTest(t *testing.T, s *Server) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(s.profile, "state", removedGuestsDir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// Only an explicit choice in the Cage window moves an Ubuntu environment:
// every file keeps its content and its place relative to state, under
// state/guests-removed, and Debian is then started in a fresh environment.
func TestSettingAsideUbuntuEnvironmentMovesItUnchangedBeforeDebian(t *testing.T) {
	s := testServer(t)
	stateDir := filepath.Join(s.profile, "state")
	owned := ubuntuEnvironmentForTest(t, s, "ubuntu-22.04")
	before := profileFilesForTest(t, s.profile)
	stopped, started := 0, 0
	s.deviceReset = deviceResetHooks{
		stopEnv:  func() error { stopped++; return nil },
		startEnv: func(bool) error { started++; return nil },
	}
	var queried []string
	s.abandonedVMState = func(_ context.Context, name string) (bool, error) {
		queried = append(queried, name)
		return false, nil
	}
	if got := s.abandonedEnvironmentSystem(); got != "ubuntu-22.04" {
		t.Fatalf("Ubuntu environment not recognised: %q", got)
	}

	// The agent and other pages without the desktop's proof cannot do it.
	r := httptest.NewRequest(http.MethodPost, s.origin+"/api/start", strings.NewReader(`{"set_aside_abandoned":true}`))
	r.Header.Set("Origin", s.origin)
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
	untrusted := httptest.NewRecorder()
	s.handler().ServeHTTP(untrusted, r)
	if untrusted.Code == http.StatusOK || s.jobs.snapshot().ID != "" {
		t.Fatalf("set aside without the Cage window: %d %s", untrusted.Code, untrusted.Body.String())
	}
	for _, body := range []string{`{"set_aside_abandoned":true,"guest_system":"ubuntu-22.04"}`, `{"set_aside_abandoned":true,"guest_system":"debian-12"}`} {
		if response := desktopRequestForTest(s, http.MethodPost, "/api/start", []byte(body)); response.Code != http.StatusConflict || s.jobs.snapshot().ID != "" {
			t.Fatalf("%s was accepted: %d %s", body, response.Code, response.Body.String())
		}
	}
	after := profileFilesForTest(t, s.profile)
	for path, content := range before {
		if after[path] != content {
			t.Fatal("a refused request changed", path)
		}
	}

	response := desktopRequestForTest(s, http.MethodPost, "/api/start", []byte(`{"set_aside_abandoned":true}`))
	if response.Code != http.StatusOK {
		t.Fatalf("set aside refused: %d %s", response.Code, response.Body.String())
	}
	if job := waitDeviceResetJob(t, s); job.Error != "" {
		t.Fatalf("set aside failed: %s", job.Error)
	}
	if stopped != 1 || started != 1 || !slices.Contains(queried, "base") || len(queried) != 2 {
		t.Fatalf("stop=%d start=%d queried=%v", stopped, started, queried)
	}
	folders := removedGuestFoldersForTest(t, s)
	if len(folders) != 1 || !strings.HasPrefix(filepath.Base(folders[0]), "ubuntu-22.04-") {
		t.Fatalf("not set aside in one folder: %v", folders)
	}
	for _, rel := range owned {
		original := filepath.Join(stateDir, rel)
		if _, err := os.Lstat(original); !os.IsNotExist(err) {
			t.Fatal("left in place:", rel, err)
		}
		if data, err := os.ReadFile(filepath.Join(folders[0], rel)); err != nil || string(data) != before[original] {
			t.Fatal("not moved unchanged:", rel, err)
		}
	}
	for _, rel := range []string{filepath.Join("lima", "_config", "user"), filepath.Join("guest-media", "debian-13.txt"), "device-reset-last.json"} {
		if data, err := os.ReadFile(filepath.Join(stateDir, rel)); err != nil || string(data) != "content of "+rel {
			t.Fatal("moved a file of no guest:", rel, err)
		}
	}
	profileFile := filepath.Join(s.profile, guestprofile.Filename)
	if data, err := os.ReadFile(profileFile); err != nil || string(data) != before[profileFile] {
		t.Fatal("the account settings did not stay for Debian", err)
	}
	if data, err := os.ReadFile(filepath.Join(folders[0], "profile", guestprofile.Filename)); err != nil || string(data) != before[profileFile] {
		t.Fatal("the account settings were not copied with the old environment", err)
	}
	var manifest removedGuestManifest
	data, err := os.ReadFile(filepath.Join(folders[0], "manifest.json"))
	if err != nil || json.Unmarshal(data, &manifest) != nil || manifest.System != "ubuntu-22.04" || len(manifest.Moved) == 0 {
		t.Fatalf("manifest: %s %v", data, err)
	}
	if found := s.abandonedGuestList(); len(found) != 0 || s.abandonedEnvironmentSystem() != "" {
		t.Fatalf("still reported after setting aside: %v", found)
	}
	if system, err := s.installedLinuxSystem(); err != nil || system != guestos.Debian13 {
		t.Fatalf("next start does not create Debian: %q %v", system, err)
	}
	// Nothing is left to set aside.
	if response := desktopRequestForTest(s, http.MethodPost, "/api/start", []byte(`{"set_aside_abandoned":true}`)); response.Code != http.StatusConflict {
		t.Fatalf("set aside twice: %d %s", response.Code, response.Body.String())
	}
}

// A machine of the old environment that still runs keeps everything where
// it is.
func TestSettingAsideRunningUbuntuEnvironmentChangesNothing(t *testing.T) {
	s := testServer(t)
	ubuntuEnvironmentForTest(t, s, "ubuntu-24.04")
	before := profileFilesForTest(t, s.profile)
	s.deviceReset = deviceResetHooks{
		stopEnv:  func() error { return nil },
		startEnv: func(bool) error { t.Error("started with the old environment in place"); return nil },
	}
	s.abandonedVMState = func(_ context.Context, name string) (bool, error) { return name == "base", nil }
	if response := desktopRequestForTest(s, http.MethodPost, "/api/start", []byte(`{"set_aside_abandoned":true}`)); response.Code != http.StatusOK {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	if job := waitDeviceResetJob(t, s); !strings.Contains(job.Error, "仍在运行") {
		t.Fatalf("running machine not reported: %q", job.Error)
	}
	after := profileFilesForTest(t, s.profile)
	// Every start records the network intent; nothing else is written.
	delete(after, filepath.Join(s.profile, "network-intent.json"))
	if len(after) != len(before) {
		t.Fatalf("files changed: %d -> %d", len(before), len(after))
	}
	for path, content := range before {
		if after[path] != content {
			t.Fatal("changed", path)
		}
	}
	if folders := removedGuestFoldersForTest(t, s); len(folders) != 0 {
		t.Fatalf("left a folder: %v", folders)
	}
}

// The programs started after the check, such as Debian's setup, take an
// inherited LIMA_HOME for the user's own Lima home and refuse to run.
func TestAbandonedVMCheckLeavesLimaHomeAlone(t *testing.T) {
	s := testServer(t)
	user := t.TempDir()
	t.Setenv("LIMA_HOME", user)
	if running, err := s.abandonedVMRunning(context.Background(), "base"); err != nil || running {
		t.Fatal(running, err)
	}
	if got := os.Getenv("LIMA_HOME"); got != user {
		t.Fatalf("LIMA_HOME left at %q", got)
	}
	os.Unsetenv("LIMA_HOME")
	if running, err := s.abandonedVMRunning(context.Background(), "base"); err != nil || running {
		t.Fatal(running, err)
	}
	if got, set := os.LookupEnv("LIMA_HOME"); set {
		t.Fatalf("LIMA_HOME left at %q", got)
	}
}

// An Ubuntu installation that already created its disk is set aside with
// its record, which otherwise keeps every start refused.
func TestSettingAsideStartedUbuntuInstallation(t *testing.T) {
	s := testServer(t)
	install := guestos.Installation{Version: 1, ID: uuid.New(), System: guestos.Ubuntu2204,
		Source: ubuntuPlanForTest(s.profile, guestos.Ubuntu2204), SourceKind: guestos.LinuxImageSource}
	if err := guestos.SaveInstallation(s.profile, install); err != nil {
		t.Fatal(err)
	}
	record, err := os.ReadFile(filepath.Join(s.profile, "guest-install.json"))
	if err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(s.profile, "state", "lima", "base-building", "diffdisk")
	if err := os.MkdirAll(filepath.Dir(disk), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(disk, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.deviceReset = deviceResetHooks{stopEnv: func() error { return nil }, startEnv: func(bool) error { return nil }}
	s.abandonedVMState = func(context.Context, string) (bool, error) { return false, nil }
	if response := desktopRequestForTest(s, http.MethodPost, "/api/start", []byte(`{"set_aside_abandoned":true}`)); response.Code != http.StatusOK {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	if job := waitDeviceResetJob(t, s); job.Error != "" {
		t.Fatalf("set aside failed: %s", job.Error)
	}
	folders := removedGuestFoldersForTest(t, s)
	if len(folders) != 1 {
		t.Fatalf("folders: %v", folders)
	}
	if data, err := os.ReadFile(filepath.Join(folders[0], "lima", "base-building", "diffdisk")); err != nil || string(data) != "partial" {
		t.Fatal("disk not moved", err)
	}
	if data, err := os.ReadFile(filepath.Join(folders[0], "profile", "guest-install.json")); err != nil || string(data) != string(record) {
		t.Fatal("record not kept with the environment", err)
	}
	if pending, err := guestos.LoadInstallation(s.profile); err != nil || pending != nil {
		t.Fatal("record still pending", pending, err)
	}
	if found := s.abandonedGuestList(); len(found) != 0 {
		t.Fatalf("still reported: %v", found)
	}
}
