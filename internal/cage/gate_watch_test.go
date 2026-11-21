package cage

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/guestprofile"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
)

// The watcher reuses a decision only for byte-identical proxy stores. Every
// change that alters the decision must still close the gate on the next tick,
// including rewrites that keep the size and modification time.
func TestGateWatcherDecidesEveryProxyStoreChangeWithinOneTick(t *testing.T) {
	profile := t.TempDir()
	if _, err := guestprofile.Create(profile, guestprofile.Options{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(profile, "config.json")
	instance := filepath.Join(profile, "state", "lima", "qa")
	if err := os.MkdirAll(instance, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.ProxyRevision = "revision-a"
	p, _ := managedproxy.Load(profile)
	if err := p.Import("qa", "", []byte(`proxies: [{name: qa, type: socks5, server: 203.0.113.10, port: 1}]`), "qa", 0, ""); err != nil {
		t.Fatal(err)
	}
	p.Selected = p.Sources[0].Nodes[0].ID
	p.Revision = cfg.ProxyRevision
	cfg.ProxyURL = p.Endpoint()
	if err := p.Save(profile); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewEgressGate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	stop, err := StartGateControl(path, instance, g)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	store := filepath.Join(managedproxy.Dir(profile), "sources.json")
	original, err := os.ReadFile(store)
	if err != nil {
		t.Fatal(err)
	}
	restore := func() {
		t.Helper()
		if err := os.WriteFile(store, original, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Two ticks with an unchanged store keep the gate open (the reused decision).
	openFor := func() {
		t.Helper()
		fixtureGrant(t, g)
		time.Sleep(2200 * time.Millisecond)
		if g.Status().Mode == GateClosed {
			t.Fatalf("unchanged proxy store closed the gate: %s", g.Status().Reason)
		}
	}
	// A watcher tick runs every second; allow one full tick plus scheduling.
	closesWithin := func(change string) {
		t.Helper()
		deadline := time.Now().Add(1500 * time.Millisecond)
		for g.Status().Mode != GateClosed && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if g.Status().Mode != GateClosed {
			t.Fatalf("%s retained network authorization", change)
		}
	}

	openFor()
	before, _ := os.Stat(store)
	changed := bytes.Replace(original, []byte(`"revision-a"`), []byte(`"revision-b"`), 1)
	if bytes.Equal(changed, original) || len(changed) != len(original) {
		t.Fatal("fixture revision not found")
	}
	f, err := os.OpenFile(store, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteAt(changed, 0); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err = os.Chtimes(store, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(store); after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("in-place rewrite changed size or modification time")
	}
	closesWithin("an in-place rewrite with the same size and modification time")

	restore()
	openFor()
	p.Selected = ""
	if err = p.Save(profile); err != nil { // atomic replacement
		t.Fatal(err)
	}
	closesWithin("a replaced proxy store")

	restore()
	openFor()
	if err = os.Remove(store); err != nil {
		t.Fatal(err)
	}
	closesWithin("a removed proxy store")

	restore()
	openFor()
}
