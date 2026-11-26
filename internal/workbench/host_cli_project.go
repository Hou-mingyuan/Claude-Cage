package workbench

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/paths"
)

// A process holds additional mounts until it exits, including a detached PTY.
// The current project itself is an ordinary persistent workbench project.
type hostCLIUse struct {
	account     string
	profile     string
	changesAuth bool
	workspace   string
	folders     []string
	extra       []string
	cancel      context.CancelFunc
}

func nativeCLIProfile(account claudeSelection) string {
	if account.GuestID != "" {
		return account.GuestID
	}
	return "default"
}

func (s *Server) checkCLIAccountUse(account claudeSelection, changing bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for use := range s.hostCLIUses {
		if use.profile == nativeCLIProfile(account) && (changing || use.changesAuth) {
			return errors.New("该账号的 CLI 任务与登录变更不能同时执行，请在当前任务结束后重试")
		}
	}
	return nil
}

// Caller holds the environment operation lock. A lock during a mount must
// not turn into a persisted directory grant after the user has locked Cage.
func (s *Server) shareCLIProject(ctx context.Context, dir string) error {
	s.mu.Lock()
	previous := s.projectShares[dir]
	s.mu.Unlock()
	if err := s.ensureProjectShare(dir); err != nil {
		return err
	}
	s.account.mu.Lock()
	err := errMobileLocked
	if s.mobileAuthorized(ctx) {
		err = s.rememberSharedProject(dir)
	}
	s.account.mu.Unlock()
	if err != nil && previous == nil {
		s.detachProjectFolders([]string{dir})
	}
	return err
}

func (s *Server) registerCLIUse(use *hostCLIUse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hostCLIUses == nil {
		s.hostCLIUses = make(map[*hostCLIUse]bool)
	}
	s.hostCLIUses[use] = true
}

// Caller holds op. A temporary mount may have been made persistent, or acquired
// by another process, after this command started.
func (s *Server) releaseCLIUse(use *hostCLIUse) {
	s.mu.Lock()
	delete(s.hostCLIUses, use)
	var unused []string
	for _, path := range use.extra {
		used := false
		for other := range s.hostCLIUses {
			if slices.Contains(other.extra, path) {
				used = true
				break
			}
		}
		if !used {
			unused = append(unused, path)
		}
	}
	s.mu.Unlock()
	if len(unused) == 0 {
		return
	}
	grants, err := s.projectPaths()
	if err != nil {
		return
	}
	s.detachProjectFolders(slices.DeleteFunc(unused, func(p string) bool { return slices.Contains(grants, p) }))
}

func (s *Server) releaseCLIProcess(use *hostCLIUse) {
	s.op.Lock()
	defer s.op.Unlock()
	s.releaseCLIUse(use)
}

func (s *Server) cliExtraDir(value, cwd string, use *hostCLIUse) (string, error) {
	if value == "" {
		return value, nil
	}
	hostDir := value
	if !filepath.IsAbs(hostDir) {
		if strings.HasPrefix(value, "/") {
			return value, nil
		}
		hostDir = filepath.Join(cwd, value)
	}
	resolved, err := paths.Resolve(hostDir)
	if os.IsNotExist(err) {
		return value, nil
	} // It may be a guest-only path.
	if err != nil {
		return "", err
	}
	if _, err = os.Stat(resolved); os.IsNotExist(err) {
		return value, nil
	} else if err != nil {
		return "", err
	}
	resolved, err = validateWorkspace(resolved, s.profile)
	if err != nil {
		return "", err
	}
	for _, root := range append(slices.Clone(use.folders), use.extra...) {
		if cage.StorageContains(root, resolved) {
			return guestPath(resolved), nil
		}
	}
	grants, err := s.projectPaths()
	if err != nil {
		return "", err
	}
	root, _ := sharedRoot(runProjects{Projects: grants}, resolved)
	if root != "" {
		if err := s.ensureProjectShare(root); err != nil {
			return "", err
		}
		use.folders = append(use.folders, root)
	} else {
		if err := s.ensureProjectShare(resolved); err != nil {
			return "", err
		}
		use.extra = append(use.extra, resolved)
	}
	return guestPath(resolved), nil
}

func cliInfo(args []string) bool {
	_, found := cliOption(args, "--help", "-h", "--version", "-v", "-V")
	return found
}

func cliGlobal(args []string) bool {
	if cliInfo(args) {
		return true
	}
	command, _ := cliWords(args)
	return slices.Contains([]string{"auth", "agents", "attach", "logs", "stop", "kill", "rm", "daemon", "install", "update", "upgrade", "setup-token"}, command)
}

func cliSubcommand(args []string) bool {
	command, _ := cliWords(args)
	return slices.Contains([]string{"agents", "attach", "auth", "auto-mode", "daemon", "doctor", "gateway", "import", "install", "logs", "mcp", "plugin", "plugins", "purge", "respawn", "rm", "setup-token", "stop", "kill", "update", "upgrade", "ultrareview"}, command)
}

// Identify the command and its verb without treating a prompt or an option's
// operand as an account mutation. The original argv still reaches Claude.
func cliWords(args []string) (string, string) {
	operands := make([]bool, len(args))
	end := walkCLIArgs(args, func(opt cliArg) {
		if opt.valueAt >= 0 {
			operands[opt.valueAt] = true
		}
	})
	var first string
	for i, token := range args[:end] {
		if operands[i] || strings.HasPrefix(token, "-") {
			continue
		}
		if first != "" {
			return first, token
		}
		if token == "" {
			break
		}
		first = token
	}
	return first, ""
}

// The executable still receives every token. This walk identifies just the
// options for which the host must supply a service (a PTY, a mount or resume
// ownership). An operand of another option must never grant a host directory.
type cliArg struct {
	key     string
	valueAt int
	inline  bool
	value   string
}

func walkCLIArgs(args []string, visit func(cliArg)) int {
	for i := 0; i < len(args); i++ {
		token := args[i]
		if token == "--" {
			return i
		}
		if !strings.HasPrefix(token, "-") {
			continue
		}
		key, value, inline := strings.Cut(token, "=")
		n := cliArg{key: key, valueAt: -1, inline: inline, value: value}
		if inline {
			n.valueAt = i
		}
		switch key {
		case "--add-dir", "--mcp-config", "--allowedTools", "--allowed-tools", "--disallowedTools", "--disallowed-tools", "--tools", "--betas", "--file":
			if inline {
				visit(n)
				continue
			} else if i+1 == len(args) || strings.HasPrefix(args[i+1], "-") {
				visit(n)
			}
			for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				n.inline = false
				n.valueAt = i
				n.value = args[i]
				visit(n)
			}
			continue
		case "--resume", "-r", "--from-pr", "--remote-control", "--teleport", "--worktree", "-w", "--debug", "-d", "--prompt-suggestions", "--cloud":
			if !inline && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				n.valueAt = i
				n.value = args[i]
			}
		case "--model", "--name", "-n", "--permission-mode", "--permission-prompts", "--permission-prompt-tool", "--append-system-prompt", "--system-prompt", "--append-system-prompt-file", "--system-prompt-file", "--settings", "--setting-sources", "--session-id", "--agents", "--agent", "--input-format", "--output-format", "--json-schema", "--fallback-model", "--effort", "--max-turns", "--max-budget-usd", "--max-thinking-tokens", "--debug-file", "--plugin-dir", "--plugin-url", "--allowed-agents", "--sdk-url", "--plugin-manifest", "--task-budget", "--autocompact", "--environment", "--remote-control-session-name-prefix", "--system-prompt-snapshot":
			if !inline && i+1 < len(args) {
				i++
				n.valueAt = i
				n.value = args[i]
			}
		}
		visit(n)
	}
	return len(args)
}

func cliOption(args []string, names ...string) (string, bool) {
	var value string
	var found bool
	walkCLIArgs(args, func(opt cliArg) {
		if slices.Contains(names, opt.key) {
			found = true
			value = opt.value
		}
	})
	return value, found
}

func (s *Server) cliArguments(args []string, dir string, use *hostCLIUse) ([]string, error) {
	out := slices.Clone(args)
	var err error
	var files []cliArg
	end := walkCLIArgs(args, func(opt cliArg) {
		if err != nil || opt.valueAt < 0 {
			return
		}
		path := opt.value
		switch opt.key {
		case "--add-dir":
			path, err = s.cliExtraDir(opt.value, dir, use)
		case "--settings", "--mcp-config", "--plugin-dir", "--append-system-prompt-file", "--system-prompt-file":
			files = append(files, opt)
			return
		default:
			return
		}
		if opt.inline {
			out[opt.valueAt] = opt.key + "=" + path
		} else {
			out[opt.valueAt] = path
		}
	})
	if err != nil {
		return nil, err
	}
	// Resolve paths after all explicit grants, including --add-dir appearing
	// later in argv. Inline JSON and guest-relative paths retain their meaning.
	roots := append(slices.Clone(use.folders), use.extra...)
	for _, option := range files {
		if !filepath.IsAbs(option.value) {
			continue
		}
		resolved, err := paths.Resolve(option.value)
		if err != nil {
			continue
		}
		for _, root := range roots {
			if cage.StorageContains(root, resolved) {
				out[option.valueAt] = guestPath(resolved)
				if option.inline {
					out[option.valueAt] = option.key + "=" + out[option.valueAt]
				}
				break
			}
		}
	}
	if cliSubcommand(args) || cliGlobal(args) {
		return out, nil
	}
	if _, bare := cliOption(args, "--bare"); bare {
		return out, nil
	}
	var additional []string
	folders, err := s.projectFolders(use.workspace)
	if err != nil {
		return nil, err
	}
	for _, folder := range folders {
		if !cage.StorageContains(folder, dir) {
			additional = append(additional, "--add-dir="+guestPath(folder))
		}
	}
	return slices.Insert(out, end, additional...), nil
}

// The common guest MCP connection does not identify a particular PTY. A
// project-bound tool may run only if every possible CLI caller shares that
// folder. Explicit calls from the installed plugin's own view remain separate.
func (s *Server) checkCLIHostProject(dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for use := range s.hostCLIUses {
		if (use.workspace != "" || use.profile != "") && !slices.Contains(use.folders, dir) && !slices.Contains(use.extra, dir) {
			return errors.New("多个终端使用不同项目，无法确认此工具的目标目录。请在对应项目中执行。")
		}
	}
	return nil
}

// PTYs are counted by terminalManager. A pipe has no visible tab, but still
// holds its account during scheduling, removal and reauthorization.
func (s *Server) activeAccountRuns() map[string]int {
	active := s.mobileCLI.activeAccounts()
	s.mu.Lock()
	defer s.mu.Unlock()
	for use := range s.hostCLIUses {
		if use.cancel != nil && use.account != "" {
			active[use.account]++
		}
	}
	return active
}
