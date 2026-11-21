//go:build !windows

package containerenv

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/lima-vm/lima/v2/pkg/guestidentity"
)

func TestContainerArchiveIgnoreAndValidation(t *testing.T) {
	profile := t.TempDir()
	script := filepath.Join(t.TempDir(), "fake-podman")
	logFile := filepath.Join(t.TempDir(), "calls.log")
	r := &Runtime{
		profile:  profile,
		identity: "abc123def456",
		config: Config{
			Engine:     "podman",
			Executable: script,
			Image:      "example/cage@sha256:" + strings.Repeat("a", 64),
			CPUs:       2,
			MemoryBytes: 2 << 30,
		},
	}
	id, err := guestidentity.New()
	if err != nil {
		t.Fatal(err)
	}
	idJSON := fmt.Sprintf(`{"hostname":%q,"uuid":%q,"macAddress":%q,"gatewayMacAddress":%q}`, id.Hostname, id.UUID, id.MACAddress, id.GatewayMACAddress)
	activeName := "/" + r.Name()
	archiveName := "/" + r.Name() + "-archive-deadbeef1234"
	inspectBoth := fmt.Sprintf(
		`[{"Id":"c-active","Name":%q,"State":{"Running":false},"Config":{"Hostname":%q,"Labels":{%q:%q,%q:%q}}},`+
			`{"Id":"c-arch","Name":%q,"State":{"Running":false},"Config":{"Hostname":%q,"Labels":{%q:%q,%q:%q}}}]`,
		activeName, id.Hostname, profileLabel, r.identity, guestIdentityLabel, idJSON,
		archiveName, id.Hostname, profileLabel, r.identity, guestIdentityLabel, idJSON,
	)
	inspectArchive := fmt.Sprintf(
		`[{"Id":"c-arch","Name":%q,"State":{"Running":false},"Config":{"Hostname":%q,"Labels":{%q:%q,%q:%q}}}]`,
		archiveName, id.Hostname, profileLabel, r.identity, guestIdentityLabel, idJSON,
	)
	body := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
if [ "$1" = "container" ] && [ "$2" = "ls" ]; then
  printf 'c-active\nc-arch\n'
  exit 0
fi
if [ "$1" = "container" ] && [ "$2" = "inspect" ]; then
  if [ "$3" = "%s-archive-deadbeef1234" ]; then
    printf '%%s\n' '%s'
    exit 0
  fi
  printf '%%s\n' '%s'
  exit 0
fi
exit 0
`, logFile, r.Name(), inspectArchive, inspectBoth)
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r.InstanceDir(), 0700); err != nil {
		t.Fatal(err)
	}
	gotID, err := r.CurrentIdentity(t.Context())
	if err != nil {
		t.Fatalf("CurrentIdentity failed with archive present: %v", err)
	}
	if gotID == nil || gotID.UUID != id.UUID || gotID.Hostname != id.Hostname {
		t.Fatalf("CurrentIdentity = %+v, want %+v", gotID, id)
	}
	if _, err := r.ArchiveCurrent(t.Context(), "invalid!id"); err == nil {
		t.Fatal("accepted invalid archive ID")
	}
	if runtime.GOOS != "linux" {
		if _, err := r.archiveContainerName("deadbeef1234"); err != nil {
			t.Fatalf("archiveContainerName: %v", err)
		}
		return
	}
	archived, err := r.ArchiveCurrent(t.Context(), "deadbeef1234")
	if err != nil {
		t.Fatalf("ArchiveCurrent: %v", err)
	}
	if archived != r.Name()+"-archive-deadbeef1234" {
		t.Fatalf("archived name = %q", archived)
	}
	if err := r.DeleteArchive(t.Context(), "deadbeef1234"); err != nil {
		t.Fatalf("DeleteArchive: %v", err)
	}
	calls, _ := os.ReadFile(logFile)
	if !strings.Contains(string(calls), "rename "+r.Name()+" "+r.Name()+"-archive-deadbeef1234") {
		t.Fatalf("missing rename call in log:\n%s", string(calls))
	}
	if !strings.Contains(string(calls), "rm -- "+r.Name()+"-archive-deadbeef1234") {
		t.Fatalf("missing rm call in log:\n%s", string(calls))
	}
}
