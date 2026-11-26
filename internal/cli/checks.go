package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/sylvinus/agent-vm/internal/mounts"
	"github.com/sylvinus/agent-vm/internal/vm"
)

// promptsDisabledBy is what turned the security questions off, if
// something did: --unsafe-disable-security-prompts, or
// AGENT_VM_UNSAFE_DISABLE_SECURITY_PROMPTS=1 in the shell. Never a file of
// the project, which the VM can write.
func (s *start) promptsDisabledBy() (string, bool) {
	switch {
	case s.opts.UnsafeNoPrompts:
		return "--unsafe-disable-security-prompts", true
	case os.Getenv("AGENT_VM_UNSAFE_DISABLE_SECURITY_PROMPTS") == "1":
		return "AGENT_VM_UNSAFE_DISABLE_SECURITY_PROMPTS=1", true
	}
	return "", false
}

// confirmUnsafe is the question after a security warning: true to go on.
// No by default, and when it cannot be asked; yes when the questions are
// off.
func (s *start) confirmUnsafe() bool {
	if by, ok := s.promptsDisabledBy(); ok {
		fmt.Fprintf(s.io.Stderr, "Continuing: %s.\n", by)
		return true
	}
	return s.ui.CanAsk() && s.ui.AskYN("Continue anyway?", false)
}

func (s *start) warnf(format string, a ...any) {
	fmt.Fprintf(s.io.Stderr, format+"\n", a...)
}

// start is one start of a project's VM.
type start struct {
	*app
	ctx  context.Context
	opts VMOpts
	name string // the VM
	dir  string // the project, as the shell spells it

	created bool // this run made the VM, and it is still there
	vmUp    bool // running when the start began, and not reset

	built      []vm.Mount // see shares
	builtFiles []mounts.FileMount
	builtKey   string
}
