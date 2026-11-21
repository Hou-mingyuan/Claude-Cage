package workbench

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sylvinus/agent-vm/internal/guestos"
	"github.com/sylvinus/agent-vm/internal/guestprofile"
	"github.com/sylvinus/agent-vm/internal/host"
	"github.com/sylvinus/agent-vm/internal/hostimage"
	"github.com/sylvinus/agent-vm/internal/state"
	"github.com/sylvinus/agent-vm/internal/vm"
)

// removedGuestsDir, in state, holds environments of systems Cage no longer
// runs that the user chose to keep while starting a new Debian environment.
// It is on the same disk as the environment, so setting one aside only
// renames its files.
const removedGuestsDir = "guests-removed"

// removedGuestManifest is written next to an environment set aside: moved
// paths keep their place relative to state, the profile's records are copied
// under profile/.
type removedGuestManifest struct {
	Version   int      `json:"version"`
	System    string   `json:"system"`
	CreatedAt int64    `json:"created_at"`
	Moved     []string `json:"moved"`
	Copied    []string `json:"copied,omitempty"`
	Removed   []string `json:"removed,omitempty"`
}

// abandonedEnvironmentSystem names the no longer supported Linux system of
// the existing environment, or of an installation an earlier version
// recorded, "" when there is none.
func (s *Server) abandonedEnvironmentSystem() string {
	if system := hostimage.AbandonedEnvironment(filepath.Join(s.profile, "state")); system != "" {
		return system
	}
	if pending, err := guestos.LoadInstallation(s.profile); err == nil && pending != nil && hostimage.AbandonedLinux(pending.System) {
		return pending.System
	}
	return ""
}

func (s *Server) abandonedVMRunning(ctx context.Context, name string) (bool, error) {
	if s.abandonedVMState != nil {
		return s.abandonedVMState(ctx, name)
	}
	inst, err := vm.InstanceIn(ctx, s.deviceResetLimaHome(), name)
	if errors.Is(err, vm.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// A Broken VM whose config is readable still has one of its processes.
	return inst.Status == vm.Running || inst.Orphaned || inst.Status == vm.Broken && inst.ConfigErr == nil, nil
}

// setAsideAbandonedEnvironment moves every file of an environment created
// from a no longer supported system to state/guests-removed/<system>-<time>,
// so that the next start creates a Debian environment. Nothing is deleted:
// on any failure everything moved so far is put back. It runs under the
// operation lock.
func (s *Server) setAsideAbandonedEnvironment() (string, error) {
	system := s.abandonedEnvironmentSystem()
	if system == "" {
		return "", errors.New("没有需要保留的旧 Ubuntu 环境")
	}
	stateDir := filepath.Join(s.profile, "state")
	s.setMessage("正在停止旧 Ubuntu 环境…")
	if err := s.hookStopEnv(); err != nil {
		return "", fmt.Errorf("停止旧 Ubuntu 环境失败，未做任何改动：%w", err)
	}
	entries, err := os.ReadDir(filepath.Join(stateDir, "lima"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var machines []string
	for _, entry := range entries {
		// _config and the other underscore folders are Lima's own, shared
		// with the next environment.
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), "_") {
			machines = append(machines, entry.Name())
		}
	}
	for _, name := range machines {
		running, err := s.abandonedVMRunning(s.operationContext(), name)
		if err != nil {
			return "", fmt.Errorf("无法确认旧客机 %s 已停止，未做任何改动：%w", name, err)
		}
		if running {
			return "", fmt.Errorf("旧客机 %s 仍在运行，请先关闭它后重试；未做任何改动", name)
		}
	}

	var moves []string
	for _, name := range machines {
		moves = append(moves, filepath.Join("lima", name))
		moves = append(moves, state.Owned(name)...)
	}
	moves = append(moves, state.BaseVersion, state.BaseBuiltBy)
	caches, _ := filepath.Glob(filepath.Join(stateDir, "guest-media", "ubuntu-*"))
	for _, cache := range caches {
		moves = append(moves, filepath.Join("guest-media", filepath.Base(cache)))
	}
	// The image choice goes last: until it has moved, the environment is
	// still refused as it was.
	for _, name := range hostimage.ChoiceFiles() {
		moves = append(moves, filepath.Join("guest-media", name))
	}
	present := moves[:0]
	for _, rel := range moves {
		if _, err := os.Lstat(filepath.Join(stateDir, rel)); err == nil {
			present = append(present, rel)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	moves = present

	// The installation record lives in the profile, which may be on another
	// disk than state: it is copied, and removed once everything moved.
	var removed []string
	if pending, err := guestos.LoadInstallation(s.profile); err == nil && pending != nil && hostimage.AbandonedLinux(pending.System) {
		removed = []string{"guest-install-media-" + pending.ID.String() + ".json", "guest-install.json"}
	}
	copied := []string{guestprofile.Filename}
	records := map[string][]byte{}
	for _, name := range append(append([]string{}, copied...), removed...) {
		data, err := os.ReadFile(filepath.Join(s.profile, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		records[name] = data
	}

	root := filepath.Join(stateDir, removedGuestsDir)
	if err := host.EnsurePrivateDir(root); err != nil {
		return "", err
	}
	base := system + "-" + time.Now().Format("20060102-150405")
	target := filepath.Join(root, base)
	for i := 2; ; i++ {
		if err = os.Mkdir(target, 0o700); !errors.Is(err, os.ErrExist) {
			break
		}
		target = filepath.Join(root, fmt.Sprintf("%s-%d", base, i))
	}
	if err != nil {
		return "", err
	}
	if err := host.EnsurePrivateDir(target); err != nil {
		_ = os.Remove(target)
		return "", err
	}
	s.setMessage("正在把旧 Ubuntu 环境原样移到 " + filepath.Join("state", removedGuestsDir, filepath.Base(target)) + " …")

	manifest := removedGuestManifest{Version: 1, System: system, CreatedAt: time.Now().Unix(), Moved: []string{}}
	var done []string
	var deleted []string
	rollback := func() {
		ok := true
		for i := len(done) - 1; i >= 0; i-- {
			if os.Rename(filepath.Join(target, done[i]), filepath.Join(stateDir, done[i])) != nil {
				ok = false
			}
		}
		for _, name := range deleted {
			if os.WriteFile(filepath.Join(s.profile, name), records[name], 0o600) != nil {
				ok = false
			}
		}
		// Keep the folder whenever anything could not be put back.
		if ok {
			_ = os.RemoveAll(target)
		}
	}
	for name, data := range records {
		path := filepath.Join(target, "profile", name)
		if err := host.EnsurePrivateDir(filepath.Dir(path)); err != nil {
			rollback()
			return "", err
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			rollback()
			return "", err
		}
		manifest.Copied = append(manifest.Copied, name)
	}
	for _, rel := range moves {
		destination := filepath.Join(target, rel)
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			rollback()
			return "", err
		}
		if err := os.Rename(filepath.Join(stateDir, rel), destination); err != nil {
			rollback()
			return "", fmt.Errorf("移动旧 Ubuntu 环境的 %s 失败，已全部放回原处：%w", rel, err)
		}
		done = append(done, rel)
		manifest.Moved = append(manifest.Moved, filepath.ToSlash(rel))
	}
	for _, name := range removed {
		if _, ok := records[name]; ok {
			manifest.Removed = append(manifest.Removed, name)
		}
	}
	if err := writePrivate(filepath.Join(target, "manifest.json"), manifest); err != nil {
		rollback()
		return "", err
	}
	for _, name := range manifest.Removed {
		if err := os.Remove(filepath.Join(s.profile, name)); err != nil {
			rollback()
			return "", err
		}
		deleted = append(deleted, name)
	}
	return target, nil
}
