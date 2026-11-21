// SPDX-License-Identifier: Apache-2.0
package qemu

import (
	"slices"
	"testing"

	"github.com/lima-vm/lima/v2/pkg/guestidentity"
)

func TestGuestIdentityArgs(t *testing.T) {
	if got := guestIdentityArgs(nil); got != nil {
		t.Fatalf("nil identity produced %v", got)
	}
	legacy := &guestidentity.Identity{UUID: "12345678-1234-4123-8123-123456789abc"}
	wantLegacy := []string{"-uuid", legacy.UUID}
	if got := guestIdentityArgs(legacy); !slices.Equal(got, wantLegacy) {
		t.Fatalf("legacy identity args = %v, want %v", got, wantLegacy)
	}
	full := &guestidentity.Identity{
		UUID:          "12345678-1234-4123-8123-123456789abc",
		SystemSerial:  "A1B2C3D4E5F6",
		BoardSerial:   "G7H8J9K0L1M2",
		ChassisSerial: "N3P4Q5R6S7T8",
	}
	wantFull := []string{
		"-uuid", full.UUID,
		"-smbios", "type=1,uuid=" + full.UUID + ",serial=" + full.SystemSerial,
		"-smbios", "type=2,serial=" + full.BoardSerial,
		"-smbios", "type=3,serial=" + full.ChassisSerial,
	}
	if got := guestIdentityArgs(full); !slices.Equal(got, wantFull) {
		t.Fatalf("full identity args = %v, want %v", got, wantFull)
	}
}
