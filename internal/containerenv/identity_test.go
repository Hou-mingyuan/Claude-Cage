package containerenv

import (
	"bytes"
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lima-vm/lima/v2/pkg/guestidentity"
	"github.com/sylvinus/agent-vm/internal/cage"
)

func TestGuestIdentityCreationAndLoopbackValidation(t *testing.T) {
	identity, err := guestidentity.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, engine := range []string{"docker", "podman"} {
		config := fixtureConfig()
		config.Engine = engine
		runtime := newRuntime(t.TempDir(), config)
		args := runtime.createArguments(identity)
		flags := map[string]string{}
		var label string
		for index, value := range args[:len(args)-1] {
			if value == "--hostname" || value == "--env" || value == "--add-host" {
				flags[value] = args[index+1]
			}
			if value == "--label" && strings.HasPrefix(args[index+1], guestIdentityLabel+"=") {
				label = strings.TrimPrefix(args[index+1], guestIdentityLabel+"=")
			}
		}
		if flags["--hostname"] != identity.Hostname || flags["--add-host"] != identity.Hostname+":127.0.1.1" || flags["--env"] != "container_uuid="+identity.UUID {
			t.Fatalf("%s did not use creation identity", engine)
		}
		container := &engineContainer{}
		container.Config.Hostname = identity.Hostname
		container.Config.Labels = map[string]string{guestIdentityLabel: label}
		container.HostConfig.NetworkMode, container.HostConfig.IpcMode, container.HostConfig.CgroupnsMode = "none", "private", "private"
		container.HostConfig.ExtraHosts = []string{flags["--add-host"]}
		if err := checkContainerConfiguration(container); err != nil {
			t.Fatal(err)
		}
		restored, err := containerIdentity(container)
		if err != nil || !reflect.DeepEqual(restored, identity) {
			t.Fatal("saved identity changed on reconnect")
		}
		container.HostConfig.ExtraHosts = []string{"cage:127.0.1.1"}
		if checkContainerConfiguration(container) == nil {
			t.Fatal("accepted a hostname alias outside the saved identity")
		}
		container.Config.Hostname = "other"
		if _, err := containerIdentity(container); err == nil {
			t.Fatal("accepted mismatched hostname")
		}
	}
}

func TestGuestIdentityCannotFallBackWhenInvalid(t *testing.T) {
	container := &engineContainer{}
	if identity, err := containerIdentity(container); err != nil || identity != nil {
		t.Fatal("legacy container no longer supported")
	}
	valid, err := guestidentity.New()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(valid)
	for _, text := range []string{"", "null", "{}", string(data) + " {}", strings.TrimSuffix(string(data), "}") + `,"unknown":true}`, strings.Repeat("x", 1025)} {
		container.Config.Labels = map[string]string{guestIdentityLabel: text}
		if _, err := containerIdentity(container); err == nil {
			t.Fatal("invalid identity silently fell back to the legacy identity")
		}
	}
}

func TestGuestIdentityNetworkUsesSavedMACs(t *testing.T) {
	identity, err := guestidentity.New()
	if err != nil {
		t.Fatal(err)
	}
	gate, err := cage.NewEgressGate(cage.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close("test finished")
	configuration := networkConfiguration(gate, identity)
	if configuration.GatewayMacAddress != identity.GatewayMACAddress || configuration.DHCPStaticLeases[GuestIP] != identity.MACAddress {
		t.Fatal("network and TAP identities differ")
	}
	if len(configuration.NAT) != 0 || len(configuration.Forwards) != 0 || configuration.Ec2MetadataAccess {
		t.Fatal("network isolation changed")
	}
	first, err := newNetwork(cage.DefaultConfig(), identity)
	if err != nil {
		t.Fatal(err)
	}
	first.Gate.Close("test finished")
	legacy := networkConfiguration(gate, nil)
	if legacy.GatewayMacAddress != GatewayMAC || legacy.DHCPStaticLeases[GuestIP] != GuestMAC {
		t.Fatal("legacy network identity changed")
	}
}

func TestRandomizedMACIsStillEnforcedAtFrameBoundary(t *testing.T) {
	identity, err := guestidentity.New()
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := net.ParseMAC(identity.MACAddress)
	for _, allowed := range []bool{true, false} {
		host, guest := net.Pipe()
		_ = host.SetDeadline(time.Now().Add(time.Second))
		_ = guest.SetDeadline(time.Now().Add(time.Second))
		frame := make([]byte, 64)
		source := expected
		if !allowed {
			source, _ = net.ParseMAC(GuestMAC)
		}
		copy(frame[6:12], source)
		done := make(chan error, 1)
		go func() { done <- writeFrame(guest, frame) }()
		bounded := &boundedFrameConnection{Conn: host, sourceMAC: expected}
		data := make([]byte, 128)
		count, err := bounded.Read(data)
		if allowed && (err != nil || count != len(frame)+4 || !bytes.Equal(data[4:count], frame)) {
			t.Errorf("valid randomized source rejected: %v", err)
		}
		if !allowed && err == nil {
			t.Error("old shared MAC bypassed the randomized source boundary")
		}
		host.Close()
		guest.Close()
		<-done
	}
}
