package guestidentity

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
)

func TestNewIndependentIdentities(t *testing.T) {
	names, ids, macs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for range 1000 {
		identity, err := New()
		if err != nil {
			t.Fatal(err)
		}
		if err := identity.Validate(); err != nil {
			t.Fatal(err)
		}
		for _, prefix := range []string{"cage-", "claude-", "lima-", "agent-vm-"} {
			if strings.HasPrefix(strings.ToLower(identity.Hostname), prefix) {
				t.Fatal("project prefix in guest hostname")
			}
		}
		if len(identity.Hostname) > 15 || names[identity.Hostname] || ids[identity.UUID] {
			t.Fatal("duplicate or invalid machine identity")
		}
		names[identity.Hostname], ids[identity.UUID] = true, true
		for _, value := range []string{identity.MACAddress, identity.GatewayMACAddress} {
			address, err := net.ParseMAC(value)
			if err != nil || len(address) != 6 || address[0]&3 != 2 || macs[value] {
				t.Fatal("duplicate or invalid MAC")
			}
			macs[value] = true
		}
		if len(identity.MachineID()) != 32 {
			t.Fatal("invalid systemd machine ID")
		}
		data, err := json.Marshal(identity)
		if err != nil {
			t.Fatal(err)
		}
		var restored Identity
		if err := json.Unmarshal(data, &restored); err != nil || restored != *identity {
			t.Fatal("identity did not round trip")
		}
	}
}

func TestRandomnessFailureAndMACCollision(t *testing.T) {
	// Check every truncation of a deterministic valid generation, including
	// failures after the hostname has already consumed its random bytes.
	random := bytes.NewReader(make([]byte, 128))
	identity, err := generate(random)
	consumed := 128 - random.Len()
	for size := range consumed {
		if _, err := generate(bytes.NewReader(make([]byte, size))); !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("accepted short random input of %d bytes: %v", size, err)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsUnsafeIdentity(t *testing.T) {
	identity, err := New()
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Identity){
		func(i *Identity) { i.Hostname = "bad;command" },
		func(i *Identity) { i.Hostname = strings.Repeat("a", 16) },
		func(i *Identity) { i.UUID = "not-a-uuid" },
		func(i *Identity) { i.MACAddress = "01:00:00:00:00:00" },
		func(i *Identity) { i.MACAddress = "00:00:00:00:00:00" },
		func(i *Identity) { i.GatewayMACAddress = i.MACAddress },
	} {
		broken := *identity
		mutate(&broken)
		if broken.Validate() == nil {
			t.Fatal("accepted invalid identity")
		}
	}
	var absent *Identity
	if absent.Validate() == nil {
		t.Fatal("accepted nil identity")
	}
}
