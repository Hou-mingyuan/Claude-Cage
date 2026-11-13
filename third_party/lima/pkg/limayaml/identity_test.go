package limayaml

import (
	"context"
	"github.com/goccy/go-yaml"
	"testing"

	"github.com/lima-vm/lima/v2/pkg/guestidentity"
	"github.com/lima-vm/lima/v2/pkg/limatype"
)

func TestGuestIdentityYAMLRoundTripAndFallback(t *testing.T) {
	identity, err := guestidentity.New()
	if err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(limatype.LimaYAML{GuestIdentity: identity, Images: []limatype.Image{{File: limatype.File{Location: "https://example.com/disk.img", Arch: "x86_64"}}}})
	if err != nil {
		t.Fatal(err)
	}
	config, err := LoadWithWarnings(context.Background(), data, "/tmp/test/lima.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(config, false); err != nil {
		t.Fatal(err)
	}
	if GuestHostname(config, "management-name") != identity.Hostname || GuestMACAddress(config, "/tmp/test") != identity.MACAddress {
		t.Fatal("identity was ignored")
	}
	config.GuestIdentity = nil
	if GuestHostname(config, "legacy") != "lima-legacy" || GuestMACAddress(config, "/tmp/test") != MACAddress("/tmp/test") {
		t.Fatal("legacy defaults changed")
	}
	config.GuestIdentity = identity
	config.GuestIdentity.Hostname = "bad;command"
	if Validate(config, false) == nil {
		t.Fatal("invalid identity accepted")
	}
}
