package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sylvinus/agent-vm/internal/env"
	"github.com/sylvinus/agent-vm/internal/mounts"
	"github.com/sylvinus/agent-vm/internal/paths"
	"github.com/sylvinus/agent-vm/internal/state"
	"github.com/sylvinus/agent-vm/internal/version"
	"github.com/sylvinus/agent-vm/internal/vm"
	"github.com/sylvinus/agent-vm/internal/vmname"
)

// refs are the locations no share may reach (see mounts.Refs).
func (e *app) refs() []mounts.Ref {
	self := ""
	if exe, err := os.Executable(); err == nil {
		if p, err := paths.Resolve(exe); err == nil {
			self = paths.Host(filepath.Dir(p))
		}
	}
	private := []string{LimaHome(e.state), oldLimaHome()}
	private = append(private, e.privateStorage...)
	return mounts.Refs(paths.Home(), self, string(e.state), private...)
}

// entries are the usable entries of ~/.agent-vm/volumes for the project
// dir, their warnings on warn.
func (e *app) entries(dir string, warn io.Writer) []mounts.Entry {
	return mounts.Entries(e.state.Path("volumes"), dir, paths.Home(), e.refs(), warn)
}

func isCageMode() bool { return os.Getenv("CLAUDE_CAGE_CONFIG") != "" }

// tristate is a key's value: 1, 0, or unknown when the backend could not be
// asked, never a confident 0.
func tristate(ok bool, err error) string {
	switch {
	case err != nil:
		return "unknown"
	case ok:
		return "1"
	}
	return "0"
}

// `info [dir]`: machine-readable state, one key=value per line, the
// supported way for another tool to ask what agent-vm knows. Booleans are
// 1/0; what cannot be told is "unknown".
//
// security_questions: what a start with writable shares of a stopped VM
// would stop on unless answered yes, comma-separated, or "none": hooks (a
// hooks folder that cannot be protected), git-config (git config or commands
// in a share).
func (e *app) infoCmd(ctx context.Context, args []string) int {
	d := ""
	if len(args) > 0 {
		d = args[0]
	}
	dir, err := vmname.AbsDir(d)
	if err != nil {
		fmt.Fprintf(e.io.Stderr, "Error: %v\n", err)
		return 1
	}
	name := e.instanceName(dir)
	w := e.io.Stdout
	fmt.Fprintf(w, "version=%s\n", version.Version)
	fmt.Fprintf(w, "template=%s\n", vmname.Template)
	fmt.Fprintf(w, "state_dir=%s\n", e.state)
	fmt.Fprintf(w, "project_env=%s\n", env.ProjectEnvFile(dir))
	fmt.Fprintf(w, "dir=%s\n", dir)
	fmt.Fprintf(w, "vm_name=%s\n", name)

	b, berr := e.readOnlyBackend()
	get := func(n string) (*vm.Instance, error) {
		if berr != nil {
			return nil, berr
		}
		inst, err := b.Get(ctx, n)
		if errors.Is(err, vm.ErrNotFound) {
			return nil, nil
		}
		return inst, err
	}
	base, baseErr := get(vmname.Template)
	_, markErr := os.Stat(e.state.Path(state.BaseVersion))
	fmt.Fprintf(w, "base_exists=%s\n", tristate(base != nil && markErr == nil, baseErr))
	inst, instErr := get(name)
	fmt.Fprintf(w, "vm_exists=%s\n", tristate(inst != nil, instErr))
	fmt.Fprintf(w, "vm_running=%s\n", tristate(inst != nil && inst.Status == vm.Running, instErr))
	stale := "unknown"
	if inst != nil {
		stale = e.staleState(name)
	}
	fmt.Fprintf(w, "vm_stale=%s\n", stale)
	// Lima names the alias after the instance.
	fmt.Fprintf(w, "ssh_host=lima-%s\n", name)
	cfg := "unknown"
	if inst != nil && inst.SSHConfig != "" {
		cfg = inst.SSHConfig
	}
	fmt.Fprintf(w, "ssh_config=%s\n", cfg)

	fmt.Fprintf(w, "git_protected=0\n")
	fmt.Fprintf(w, "security_questions=none\n")
	return 0
}

// staleState says whether vm was cloned from an older base than the
// current one: 1, 0, or unknown with nothing to compare against. A VM with
// no record but a known base predates the record, which makes it stale.
func (e *app) staleState(name string) string {
	base, err := os.ReadFile(e.state.Path(state.BaseVersion))
	if err != nil {
		return "unknown"
	}
	mine, err := os.ReadFile(e.state.Marker(state.VersionOf, name))
	if err != nil || strings.TrimRight(string(mine), "\n") != strings.TrimRight(string(base), "\n") {
		return "1"
	}
	return "0"
}
