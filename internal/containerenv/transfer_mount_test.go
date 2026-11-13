package containerenv

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/sylvinus/agent-vm/internal/cage"
)

func TestNetworkRecordRejectsStaleTransferRootBeforeConnecting(t *testing.T) {
	r := newRuntime(t.TempDir(), fixtureConfig())
	for _, root := range []string{"", t.TempDir()} {
		record := networkRecord{ContainerID: "retained-container", RunID: "running", Shared: true, SharedDirectory: root}
		if err := writePrivateJSON(filepath.Join(r.InstanceDir(), "network.json"), record); err != nil {
			t.Fatal(err)
		}
		if _, err := r.networkState(context.Background(), record.ContainerID); !errors.Is(err, errSharedDirectoryChanged) {
			t.Fatalf("stale transfer root was accepted: %q %v", root, err)
		}
	}
	// A current root proceeds to independent endpoint validation, rather than
	// pretending a root match proves the worker or network is healthy.
	record := networkRecord{ContainerID: "retained-container", RunID: "running", Shared: true, SharedDirectory: cage.EnvironmentDir(r.profile)}
	if err := writePrivateJSON(filepath.Join(r.InstanceDir(), "network.json"), record); err != nil {
		t.Fatal(err)
	}
	if _, err := r.networkState(context.Background(), record.ContainerID); err == nil || errors.Is(err, errSharedDirectoryChanged) {
		t.Fatalf("current transfer root skipped endpoint validation: %v", err)
	}
}
