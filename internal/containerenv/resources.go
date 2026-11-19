package containerenv

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// PIDLimit counts processes and threads together. A browser, an editor and a
// few parallel agent CLIs already pass a thousand threads between them.
const PIDLimit = 16384

type resourceEvidence struct {
	MemoryBytes int64 `json:"memory_bytes"`
	CPUs        int   `json:"cpus"`
	PIDs        int64 `json:"pids"`
}

// cgroupLimits holds one cgroup's memory.max, cpu.max and pids.max as read.
type cgroupLimits struct{ memory, cpu, pids string }

func (r *Runtime) resourceSlice() string { return "cage" + r.identity + ".slice" }

// Read the host-owned parent cgroup, outside the guest's writable namespace,
// and verify that systemd placed this exact container scope underneath it.
func (r *Runtime) verifyResources(ctx context.Context, containerID string) (resourceEvidence, error) {
	return r.verifyHostResources(ctx, containerID)
}

// Create writes --pids-limit, --cpus and --memory into the container scope
// once, while the parent slice is re-applied from the configuration on every
// start. Without this update an existing container keeps its create-time
// values, and the lowest limit on the path applies. A failed update refuses
// the start instead of running with limits Cage did not verify.
func (r *Runtime) updateResources(ctx context.Context, containerID string) error {
	container, err := r.inspectContainer(ctx)
	if err != nil {
		return err
	}
	if container == nil || container.ID != containerID {
		return errors.New("容器身份已变化，请重新启动环境")
	}
	args := resourceUpdate(container, r.config)
	if args == nil {
		return nil
	}
	if _, err := r.output(ctx, args...); err != nil {
		return fmt.Errorf("无法把已有容器的资源限额更新为当前配置，已拒绝启动: %w", err)
	}
	return nil
}

// resourceUpdate returns the engine arguments that bring a container's
// create-time limits to the current values, or nil when they already match.
func resourceUpdate(container *engineContainer, configuration Config) []string {
	host := container.HostConfig
	if host.PidsLimit != nil && *host.PidsLimit == PIDLimit && host.NanoCpus == int64(configuration.CPUs)*1e9 && host.Memory == configuration.MemoryBytes {
		return nil
	}
	// Create passes --memory alone, so the engine sets memory plus swap to
	// twice the memory. Keep that ratio; raising --memory past the old total
	// without --memory-swap is refused by the engine.
	return []string{"update", "--pids-limit", strconv.Itoa(PIDLimit), "--cpus", strconv.Itoa(configuration.CPUs),
		"--memory", strconv.FormatInt(configuration.MemoryBytes, 10), "--memory-swap", strconv.FormatInt(2*configuration.MemoryBytes, 10), container.ID}
}

// The slice is the hard ceiling: the guest cannot see it. The container scope
// is checked as well because the lowest limit on the path applies, so a value
// left from creation would cap the container below the slice while the slice
// check passes. The scope is the root of the guest's cgroup namespace; with
// nsdelegate the kernel rejects writes to a namespace root's files from inside
// the namespace, except those listed in /sys/kernel/cgroup/delegate, so the
// guest cannot change these limits and reading them is trustworthy. A nil
// scope means no container is being checked yet.
func parseResourceEvidence(slice cgroupLimits, scope *cgroupLimits, mountinfo string, configuration Config) (resourceEvidence, error) {
	evidence, err := parseLimits(slice, configuration)
	if err != nil {
		return resourceEvidence{}, err
	}
	if scope != nil {
		if _, err := parseLimits(*scope, configuration); err != nil {
			return resourceEvidence{}, errors.New("容器自身的 CPU、内存或进程数限额与配置不一致；请停止环境后重新启动，Cage 会更新已有容器")
		}
	}
	cgroup2 := false
	for _, mount := range strings.Split(mountinfo, "\n") {
		fields := strings.Fields(mount)
		if len(fields) > 6 && fields[4] == "/sys/fs/cgroup" && strings.Contains(mount, " - cgroup2 ") {
			cgroup2 = true
		}
	}
	if !cgroup2 {
		return resourceEvidence{}, errors.New("容器资源限额缺少 cgroup v2 内核挂载证据")
	}
	return evidence, nil
}

func parseLimits(limits cgroupLimits, configuration Config) (resourceEvidence, error) {
	memory, memoryErr := strconv.ParseInt(limits.memory, 10, 64)
	cpu := strings.Fields(limits.cpu)
	pids, pidErr := strconv.ParseInt(limits.pids, 10, 64)
	if memoryErr != nil || pidErr != nil || len(cpu) != 2 {
		return resourceEvidence{}, errors.New("容器 CPU、内存或进程数限额未生效")
	}
	quota, quotaErr := strconv.ParseInt(cpu[0], 10, 64)
	period, periodErr := strconv.ParseInt(cpu[1], 10, 64)
	if quotaErr != nil || periodErr != nil || period < 1000 || period > 1000000 || quota != int64(configuration.CPUs)*period || memory != configuration.MemoryBytes || pids != PIDLimit {
		return resourceEvidence{}, errors.New("容器内核资源限额与配置不一致，拒绝继续启动")
	}
	return resourceEvidence{MemoryBytes: memory, CPUs: configuration.CPUs, PIDs: pids}, nil
}
