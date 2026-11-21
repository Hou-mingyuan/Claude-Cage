package managedproxy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEndpointAloneCannotAuthorizeNetworking(t *testing.T) {
	profile := t.TempDir()
	p, _ := Load(profile)
	if _, err := Applied(profile, "socks5://127.0.0.1:17896", ""); err == nil {
		t.Fatal("old endpoint accepted without nodes")
	}
	if err := p.Import("qa", "", []byte(`proxies: [{name: qa, type: socks5, server: 203.0.113.10, port: 1080}]`), "qa", 0, ""); err != nil {
		t.Fatal(err)
	}
	p.Revision = "applied"
	p.Selected = p.Sources[0].Nodes[0].ID
	if err := p.Save(profile); err != nil {
		t.Fatal(err)
	}
	for _, input := range [][2]string{{p.Endpoint(), ""}, {p.Endpoint(), "old"}, {"socks5://127.0.0.1:7890", p.Revision}} {
		if _, err := Applied(profile, input[0], input[1]); err == nil {
			t.Fatal("unapplied endpoint accepted")
		}
	}
	if _, err := Applied(profile, p.Endpoint(), p.Revision); err != nil {
		t.Fatal(err)
	}
	p.Selected = ""
	_ = p.Save(profile)
	if _, err := Applied(profile, p.Endpoint(), p.Revision); err == nil {
		t.Fatal("removed node retained networking authorization")
	}
}

func TestAppliedCheckMatchesAppliedForEveryStoreState(t *testing.T) {
	profile := t.TempDir()
	p, _ := Load(profile)
	if err := p.Import("qa", "", []byte(`proxies: [{name: qa, type: socks5, server: 203.0.113.10, port: 1080}]`), "qa", 0, ""); err != nil {
		t.Fatal(err)
	}
	p.Revision, p.Selected = "applied", p.Sources[0].Nodes[0].ID
	endpoint := p.Endpoint()
	var check AppliedCheck
	same := func(step string, endpoint, revision string) {
		t.Helper()
		_, want := Applied(profile, endpoint, revision)
		for i := 0; i < 2; i++ { // the second call takes the cached path
			if got := check.Check(profile, endpoint, revision); (got == nil) != (want == nil) || got != nil && got.Error() != want.Error() {
				t.Fatalf("%s: check=%v applied=%v", step, got, want)
			}
		}
	}
	same("missing store", endpoint, "applied")
	if err := p.Save(profile); err != nil {
		t.Fatal(err)
	}
	same("applied store", endpoint, "applied")
	same("old revision", endpoint, "old")
	same("other endpoint", "socks5://127.0.0.1:7890", "applied")
	same("applied again", endpoint, "applied")
	p.Selected = ""
	_ = p.Save(profile)
	same("node removed", endpoint, "applied")
	if err := os.WriteFile(filepath.Join(Dir(profile), "sources.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	same("corrupt store", endpoint, "applied")
	_ = os.Remove(filepath.Join(Dir(profile), "sources.json"))
	same("removed store", endpoint, "applied")
}

func TestStoreReaderLoadMatchesLoad(t *testing.T) {
	profile := t.TempDir()
	var reader StoreReader
	if p, err := reader.Load(profile); err != nil || p.Version != 1 || len(p.Auth) != 48 {
		t.Fatalf("missing store: %+v %v", p, err)
	}
	p, _ := Load(profile)
	if err := p.Import("qa", "", []byte(`proxies: [{name: qa, type: socks5, server: 203.0.113.10, port: 1080}]`), "qa", 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := p.Save(profile); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		got, err := reader.Load(profile)
		if err != nil || got.Auth != p.Auth || got.Count() != 1 {
			t.Fatalf("load %d: %+v %v", i, got, err)
		}
	}
	p.Selected = p.Sources[0].Nodes[0].ID
	_ = p.Save(profile)
	if got, _ := reader.Load(profile); got.Selected != p.Selected {
		t.Fatal("changed store returned the previous parse")
	}
	_ = os.WriteFile(filepath.Join(Dir(profile), "sources.json"), []byte("{"), 0600)
	if _, err := reader.Load(profile); err == nil {
		t.Fatal("corrupt store accepted")
	}
}
