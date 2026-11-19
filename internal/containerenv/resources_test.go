package containerenv

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

const cgroupMount = "698 756 0:25 / /sys/fs/cgroup rw,relatime - cgroup2 none rw\n"

func fixtureLimits() cgroupLimits {
	return cgroupLimits{memory: "4294967296", cpu: "200000 100000", pids: strconv.Itoa(PIDLimit)}
}

func TestKernelResourceEvidenceRejectsIgnoredOrDifferentLimits(t *testing.T) {
	scope := fixtureLimits()
	if evidence, err := parseResourceEvidence(fixtureLimits(), &scope, cgroupMount, fixtureConfig()); err != nil || evidence.CPUs != 2 || evidence.MemoryBytes != 4<<30 || evidence.PIDs != PIDLimit {
		t.Fatalf("valid cgroup evidence: %+v %v", evidence, err)
	}
	for _, change := range []func(*cgroupLimits){
		func(l *cgroupLimits) { l.memory = "max" },
		func(l *cgroupLimits) { l.cpu = "max 100000" },
		func(l *cgroupLimits) { l.pids = "max" },
		func(l *cgroupLimits) { l.pids = "1024" },
		func(l *cgroupLimits) { l.cpu = "400000 100000" },
	} {
		slice := fixtureLimits()
		change(&slice)
		if _, err := parseResourceEvidence(slice, &scope, cgroupMount, fixtureConfig()); err == nil {
			t.Fatalf("accepted non-enforced slice limits %+v", slice)
		}
	}
	for _, mount := range []string{strings.Replace(cgroupMount, "cgroup2", "tmpfs", 1), strings.Replace(cgroupMount, "/sys/fs/cgroup", "/somewhere", 1)} {
		if _, err := parseResourceEvidence(fixtureLimits(), &scope, mount, fixtureConfig()); err == nil {
			t.Fatalf("accepted resource evidence without a cgroup v2 mount %q", mount)
		}
	}
}

// A container keeps the limits it was created with while the slice is
// re-applied on every start, so only the scope check can see stale values.
func TestContainerScopeKeepingCreateTimeLimitsIsRejected(t *testing.T) {
	if _, err := parseResourceEvidence(fixtureLimits(), nil, cgroupMount, fixtureConfig()); err != nil {
		t.Fatalf("slice-only evidence before a container exists: %v", err)
	}
	for _, change := range []func(*cgroupLimits){
		func(l *cgroupLimits) { l.pids = "1024" },
		func(l *cgroupLimits) { l.pids = strconv.Itoa(PIDLimit * 2) },
		func(l *cgroupLimits) { l.pids = "max" },
		func(l *cgroupLimits) { l.cpu = "100000 100000" },
		func(l *cgroupLimits) { l.memory = "2147483648" },
		func(l *cgroupLimits) { l.memory = "max" },
	} {
		scope := fixtureLimits()
		change(&scope)
		if _, err := parseResourceEvidence(fixtureLimits(), &scope, cgroupMount, fixtureConfig()); err == nil {
			t.Fatalf("accepted container scope limits %+v", scope)
		}
	}
}

func currentContainer() *engineContainer {
	container := &engineContainer{ID: "c1"}
	limit := int64(PIDLimit)
	container.HostConfig.PidsLimit, container.HostConfig.NanoCpus, container.HostConfig.Memory = &limit, 2e9, 4<<30
	return container
}

func TestResourceUpdateTouchesOnlyStaleContainers(t *testing.T) {
	if got := resourceUpdate(currentContainer(), fixtureConfig()); got != nil {
		t.Fatalf("current container: got %q", got)
	}
	want := []string{"update", "--pids-limit", strconv.Itoa(PIDLimit), "--cpus", "2", "--memory", "4294967296", "--memory-swap", "8589934592", "c1"}
	for name, change := range map[string]func(*engineContainer){
		"old PID limit":     func(c *engineContainer) { old := int64(1024); c.HostConfig.PidsLimit = &old },
		"missing PID limit": func(c *engineContainer) { c.HostConfig.PidsLimit = nil },
		"old CPUs":          func(c *engineContainer) { c.HostConfig.NanoCpus = 1e9 },
		"old memory":        func(c *engineContainer) { c.HostConfig.Memory = 2 << 30 },
	} {
		container := currentContainer()
		change(container)
		if got := resourceUpdate(container, fixtureConfig()); !slices.Equal(got, want) {
			t.Fatalf("%s: got %q", name, got)
		}
	}
}
