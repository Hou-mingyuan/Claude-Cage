package workbench

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// A first boot interrupted before its disk or installation record exists
// leaves only the instance files of state/lima/base-building (#292). That is
// not an environment of an unsupported system: the installation continues
// with Debian 13, and nothing asks for a system choice that no longer exists.
func TestInterruptedBaseBuildingContinuesWithDebian(t *testing.T) {
	s := testServer(t)
	building := filepath.Join(s.profile, "state", "lima", "base-building")
	if err := os.MkdirAll(building, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"lima.yaml", "cloud-config.yaml", "lima-version"} {
		if err := os.WriteFile(filepath.Join(building, name), []byte("partial "+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	response := desktopRequestForTest(s, http.MethodGet, "/api/environment/guest-systems", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("installation view refused: %d %s", response.Code, response.Body.String())
	}
	var view guestInstallView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	// Not fresh and nothing pending: the page continues the existing
	// environment (validGuestChoice(null)) instead of offering a choice.
	if view.Fresh || view.Pending != nil || view.Reselect || view.Reason != "" {
		t.Fatalf("interrupted base offered a choice or an explanation: %+v", view)
	}
	if found := s.abandonedGuestList(); len(found) != 0 {
		t.Fatalf("interrupted Debian base reported as an unsupported guest: %v", found)
	}
	if response := desktopRequestForTest(s, http.MethodPost, "/api/start", []byte(`{"guest_system":"ubuntu-22.04"}`)); response.Code != http.StatusConflict {
		t.Fatalf("another system accepted for the interrupted base: %d", response.Code)
	}
	for _, name := range []string{"lima.yaml", "cloud-config.yaml", "lima-version"} {
		if data, err := os.ReadFile(filepath.Join(building, name)); err != nil || string(data) != "partial "+name {
			t.Fatalf("interrupted base file %s changed: %v", name, err)
		}
	}
}
