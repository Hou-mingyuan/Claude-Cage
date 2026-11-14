package guestidentity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestHardwareCatalogSnapshot(t *testing.T) {
	catalog, err := loadHardwareCatalog()
	if err != nil {
		t.Fatal(err)
	}
	profiles := append(append([]hardwareProfile{}, catalog.computers...), catalog.motherboards...)
	models, prefixes, vendors, kinds := map[string]bool{}, map[string]bool{}, map[string]int{}, map[string]int{}
	for _, profile := range profiles {
		models[profile.model] = true
		prefixes[profile.prefix] = true
		vendors[profile.vendor]++
		kinds[profile.kind]++
	}
	if len(profiles) < 1000 || len(prefixes) < 1000 || len(vendors) != 4 || len(kinds) != 4 {
		t.Fatal("hardware catalog lost its model diversity")
	}
	for _, model := range []string{"OptiPlex 7010", "ThinkCentre M70Q Gen 5", "HP Elite Mini 800 G9 Desktop PC", "B860M Steel Legend WiFi"} {
		if !models[model] {
			t.Errorf("missing source-backed model %q", model)
		}
	}
	raw, err := os.ReadFile("hostname-sources.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Models         int            `json:"models"`
		UniquePrefixes int            `json:"unique_prefixes"`
		CatalogSHA256  string         `json:"catalog_sha256"`
		ByVendor       map[string]int `json:"by_vendor"`
		ByKind         map[string]int `json:"by_kind"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(hardwareCSV))
	if manifest.Models != len(profiles) || manifest.UniquePrefixes != len(prefixes) || manifest.CatalogSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("catalog manifest does not match embedded data")
	}
	for key, count := range vendors {
		if manifest.ByVendor[key] != count {
			t.Fatal("vendor counts do not match embedded data")
		}
	}
	for key, count := range kinds {
		if manifest.ByKind[key] != count {
			t.Fatal("kind counts do not match embedded data")
		}
	}
}

func TestHostnameStyles(t *testing.T) {
	catalog, err := loadHardwareCatalog()
	if err != nil {
		t.Fatal(err)
	}
	prefixes := []string{"DESKTOP", "LAPTOP", catalog.computers[0].prefix, catalog.computers[0].prefix, catalog.motherboards[0].prefix}
	for style, prefix := range prefixes {
		random := make([]byte, 256)
		random[0] = byte(style)
		hostname, err := generateHostname(bytes.NewReader(random))
		if err != nil {
			t.Fatal(err)
		}
		suffixLength := 6
		if style < 2 {
			suffixLength = 7
		}
		if hostname != prefix+"-"+strings.Repeat("A", suffixLength) || len(hostname) > 15 {
			t.Fatalf("unexpected hostname for style %d: %s", style, hostname)
		}
	}
}

func TestHostnameSuffixRejectsBiasedByteValues(t *testing.T) {
	suffix, err := randomHostnameSuffix(bytes.NewReader([]byte{252, 255, 0, 25, 26, 35}), 4)
	if err != nil || suffix != "AZ09" {
		t.Fatalf("unexpected suffix %q: %v", suffix, err)
	}
}

func TestHardwareHostnameDiversity(t *testing.T) {
	catalog, err := loadHardwareCatalog()
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{"DESKTOP": true, "LAPTOP": true}
	for _, profiles := range [][]hardwareProfile{catalog.computers, catalog.motherboards} {
		for _, profile := range profiles {
			known[profile.prefix] = true
		}
	}
	lengths, cases, seenPrefixes := map[int]bool{}, map[bool]bool{}, map[string]bool{}
	for range 1000 {
		identity, err := New()
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.Split(identity.Hostname, "-")
		if len(parts) != 2 || !known[strings.ToUpper(parts[0])] || len(parts[1]) < 6 || len(parts[1]) > 7 {
			t.Fatalf("hostname does not match a known naming style: %s", identity.Hostname)
		}
		lengths[len(identity.Hostname)] = true
		cases[identity.Hostname == strings.ToLower(identity.Hostname)] = true
		seenPrefixes[strings.ToUpper(parts[0])] = true
	}
	if len(lengths) < 3 || len(cases) != 2 || len(seenPrefixes) < 20 {
		t.Fatal("hostname output lost its naming diversity")
	}
}

func TestHardwareCatalogRejectsInvalidData(t *testing.T) {
	valid := "kind,vendor,model,prefix\nlaptop,Lenovo,ThinkPad T14,T14\nmotherboard,ASRock,B650M Pro RS,B650M\n"
	for _, broken := range []string{
		"", strings.Replace(valid, "kind,vendor", "vendor,kind", 1),
		strings.Replace(valid, "laptop,Lenovo", "unknown,Lenovo", 1),
		strings.Replace(valid, "laptop,Lenovo", "laptop,Unknown", 1),
		strings.Replace(valid, ",T14\n", ",BAD;NAME\n", 1),
		strings.Replace(valid, ",T14\n", ",TOOLONGNAME\n", 1),
		strings.Replace(valid, "ThinkPad T14", " ThinkPad T14", 1),
		valid + "laptop,Lenovo,ThinkPad T14,T14\n",
		"kind,vendor,model,prefix\nlaptop,Lenovo,ThinkPad T14,T14\n",
		valid + "laptop,Lenovo,unclosed,\"prefix\n",
	} {
		if _, err := parseHardwareCatalog(strings.NewReader(broken)); err == nil {
			t.Fatal("accepted malformed hardware catalog")
		}
	}
}

func TestLegacyAndOEMHostnamesRemainValid(t *testing.T) {
	identity, err := New()
	if err != nil {
		t.Fatal(err)
	}
	for _, hostname := range []string{"abcdefghijkl", "DESKTOP-A1B2C3D", "LAPTOP-A1B2C3D", "xps9530-a1b2c3", "T14G4-01AZ9F"} {
		identity.Hostname = hostname
		if err := identity.Validate(); err != nil {
			t.Fatalf("rejected portable hostname %q: %v", hostname, err)
		}
	}
}
