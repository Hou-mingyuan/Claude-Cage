package managedproxy

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRuntimeBindingChecksObserveSameSizeEdits(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.yaml")
	stamp := time.Unix(1700000000, 0)
	write := func(node string) {
		t.Helper()
		if err := os.WriteFile(file, []byte("interface-name: eth0\nproxies: ["+node+"]\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(file, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	write("{server: remote.qa}")
	if bound, err := BoundInterface(file); err != nil || bound != "eth0" || !BindingMatches(file, "eth0") {
		t.Fatalf("initial binding: %q, %v", bound, err)
	}
	write("{server: localhost}") // Same inode, timestamp and length.
	if _, err := BoundInterface(file); !errors.Is(err, ErrLocalProxyNode) || BindingMatches(file, "eth0") {
		t.Fatalf("retired local address still usable: %v", err)
	}
	write("{server: localhost, dialer-proxy: {invalid: route}}")
	if _, err := BoundInterface(file); !errors.Is(err, ErrLocalProxyNode) {
		t.Fatalf("malformed route hid a retired node: %v", err)
	}
	write("{server: remote.qa, dialer-proxy: {invalid: route}}")
	if bound, err := BoundInterface(file); err != nil || bound != "eth0" || BindingMatches(file, "eth0") {
		t.Fatalf("node binding validation: %q, %v", bound, err)
	}
	write("{") // Invalid YAML must never reuse the last valid result.
	if _, err := BoundInterface(file); err == nil || BindingMatches(file, "eth0") {
		t.Fatal("malformed file retained a binding")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, err := BoundInterface(file); !os.IsNotExist(err) || BindingMatches(file, "eth0") {
		t.Fatalf("removed file retained a binding: %v", err)
	}
}

func TestAppliedAndRuntimeRecheckAssignedAddresses(t *testing.T) {
	previous := localInterfaceAddrs
	t.Cleanup(func() { localInterfaceAddrs = previous })
	var addresses []net.Addr
	localInterfaceAddrs = func() ([]net.Addr, error) { return addresses, nil }
	profile := t.TempDir()
	p, err := Load(profile)
	if err != nil {
		t.Fatal(err)
	}
	const node = "proxies: [{name: a, type: http, server: 192.0.2.1, port: 80}]"
	if err := p.Import("a", "", []byte(node), "a", 0, ""); err != nil {
		t.Fatal(err)
	}
	p.Selected, p.Revision = p.Sources[0].Nodes[0].ID, "applied"
	if err := p.Save(profile); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(Dir(profile), "config.yaml")
	if err := os.WriteFile(file, []byte("interface-name: eth0\nproxies: [{server: 192.0.2.2}]"), 0600); err != nil {
		t.Fatal(err)
	}
	var check AppliedCheck
	checkLocal := func(wantLocal bool) {
		t.Helper()
		err := check.Check(profile, p.Endpoint(), p.Revision)
		if wantLocal && !errors.Is(err, ErrLocalProxyNode) || !wantLocal && err != nil {
			t.Fatalf("applied check with host addresses %v: %v", addresses, err)
		}
	}
	checkLocal(false)
	for _, ip := range []string{"192.0.2.1", "192.0.2.2"} {
		addresses = []net.Addr{&net.IPNet{IP: net.ParseIP(ip), Mask: net.CIDRMask(24, 32)}}
		checkLocal(true)
		addresses = nil
		checkLocal(false)
	}
	// A source-only change must reach the gate even when the core file and
	// the source file's size, inode and timestamp all remain unchanged.
	source := filepath.Join(Dir(profile), "sources.json")
	saved, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	revoked := bytes.ReplaceAll(saved, []byte("192.0.2.1"), []byte("127.0.0.1"))
	if err := os.WriteFile(source, revoked, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(source, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	checkLocal(true)
}

func TestConcurrentRuntimeBindingProfiles(t *testing.T) {
	var wg sync.WaitGroup
	defer wg.Wait()
	for _, name := range []string{"eth0", "eth1"} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("interface-name: "+name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		wg.Go(func() {
			for range 40 {
				if bound, err := BoundInterface(path); err != nil || bound != name || !BindingMatches(path, name) {
					t.Errorf("binding for %s: %q, %v", name, bound, err)
					return
				}
			}
		})
	}
}
