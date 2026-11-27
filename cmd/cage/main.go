package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	agentvm "github.com/sylvinus/agent-vm"
	"github.com/sylvinus/agent-vm/internal/appupdate"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/cli"
	"github.com/sylvinus/agent-vm/internal/containerenv"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/guestprofile"
	"github.com/sylvinus/agent-vm/internal/host"
	"github.com/sylvinus/agent-vm/internal/hostproxy"
	"github.com/sylvinus/agent-vm/internal/limaembed"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
	"github.com/sylvinus/agent-vm/internal/uilocale"
	"github.com/sylvinus/agent-vm/internal/workbench"
	"io"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
)

const help = `Claude Cage — local isolated Claude development

cage               Show this command overview
cage claude [args...]   Run Claude Code with the default account in the current
                   project. Pass Claude prompts, options and subcommands directly.
                   git diff | cage claude -p "Review this diff" --output-format json
cage <name> [args...]   Use the named Cage account for this invocation.
                   Example: cage sarah -p "Review this project"
                   Project invocations share the current folder automatically.
cage start         Start Cage in the background and wait for its local environment
                   --reset re-clones the VM; files and logins on its disk are lost
cage status        Show the state of the background service and the environment
cage account list  Show account numbers, stable IDs, names, Claude emails and status
cage account use SELECTOR|auto    Select a default CLI account or use workbench scheduling
cage account run SELECTOR [--] ARGS   Use an account just for this invocation
cage account rename SELECTOR NAME     Change the local name used by cage <name>
cage restart       Restart the local Cage service and environment using the existing disk
                   Running tasks are interrupted.
cage update [--check] [--yes]   Update the installed Cage application
                   --check only checks for a release. --yes confirms installation.
                   Save your work and run cage stop yourself if the environment is in use.
cage update-tools  Update the tools installed inside the environment
cage install-cli   Install the terminal command for this user
cage desktop       Open the unified Cage workbench (desktop launcher)
cage agent         Discover and use the versioned, JSON-only external Agent interface
cage serve         Run the private localhost control service in the foreground
cage server-entry --json  Issue a private SSH remote connection entry
cage container-configure --json  Configure a Linux container from JSON stdin
cage init          Create a private profile; add and select nodes in Cage settings
cage check         Verify proxy transport (no VM required)
cage guest-os --json  Check host prerequisites for a same-system guest
cage proxy-start   Start Cage's bundled proxy core with the applied node
cage proxy-stop    Stop Cage's core and close external networking
cage setup         Build the isolated base VM through the specified proxy
cage up            Open or bring forward the Cage desktop window
cage login         Sign in to your Claude account in Cage's browser for Claude Code
                   --reset re-clones the VM before Claude sign-in; files and logins on its disk are lost
cage access        Inspect legacy viewer credentials (diagnostics only)
cage shell         Open a shell in the shared environment
cage run COMMAND   Execute a command in the shared environment: from the current
                   directory's place when it is in a shared project, otherwise
                   from the environment directory (with a notice on stderr).
                   --here requires the current directory (asks before sharing
                   it on a terminal, fails otherwise); --workdir DIR runs from
                   DIR in the environment
cage stop          Stop the local environment, its tasks, proxy and background service
cage list          List this cage profile's VMs
cage doctor        Diagnose the VM
cage diagnostics --json PATH.zip   Export a redacted diagnostic bundle
cage version       Print this build's version. --min VERSION fails if older
cage licenses      Print third-party license notices

Configuration: ~/.claude-cage/config.json (override CLAUDE_CAGE_CONFIG).
Each configuration directory is an independent cage profile.
Project commands share their current folder. Management commands do not.
`

func main() {
	defer func() {
		if failure := recover(); failure != nil {
			diagnostics.Log("error", "command", fmt.Sprintf("panic: %v\n%s", failure, debug.Stack()))
			panic(failure)
		}
	}()
	code := run()
	if code != 0 {
		diagnostics.Log("error", "command", "operation exited unsuccessfully")
	}
	os.Exit(code)
}
func run() int {
	hostproxy.CaptureEnvironment()
	// Apply this before desktop/serve dispatch: the control service and every
	// child process must keep Cage's isolation even when launched from a shell
	// configured to relax agent-vm's protections.
	for _, name := range []string{
		"AGENT_VM_UNSAFE_OPEN_NETWORK",
		"AGENT_VM_UNSAFE_DISABLE_SECURITY_PROMPTS",
	} {
		os.Unsetenv(name)
	}
	// The shared commands otherwise name agent-vm and suggest its installer.
	cli.Command = "claude-cage"
	cli.UpdateHint = "run 'cage update' to update Cage"
	// --reset deletes the environment's disk with the user's files and logins.
	cli.StaleBaseHint = "提示：Cage 的基础镜像比当前环境新。当前环境的磁盘会保留，不需要重建；请在 Cage 的“设置 > 环境与恢复”中更新环境组件或环境工具，也可以运行 cage update-tools。"
	args, namedAccount, err := cageArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Print(help)
		return 0
	}
	if !namedAccount {
		if text, ok := subcommandHelp(args); ok {
			fmt.Print(text)
			return 0
		}
	}
	var updateOptions hostUpdateOptions
	if args[0] == "update" {
		var err error
		updateOptions, err = parseHostUpdateArgs(args[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	if args[0] == "up" && len(args) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: cage up")
		if slices.Contains(args[1:], "--reset") {
			fmt.Fprintln(os.Stderr, "To rebuild the VM, use cage start --reset. Files and logins on its disk will be lost.")
		}
		return 2
	}
	var startArgs []string
	if args[0] == "start" || args[0] == "login" || args[0] == "vm-start" {
		var err error
		startArgs, err = cli.StartArgs(args[1:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\nUsage: cage %s [--reset]\n", err, args[0])
			return 2
		}
	}
	// These read only this build's own metadata. Answering them before the
	// profile is opened keeps `cage version --min` usable on a fresh install.
	if args[0] == "version" || args[0] == "--version" || args[0] == "-V" || args[0] == "licenses" {
		return cli.Main(args, cli.Stdio())
	}
	if args[0] == "agent" {
		return runAgent(args[1:], os.Stdin, os.Stdout)
	}
	if args[0] == "verify-web-assets" {
		if len(args) != 1 {
			return 2
		}
		if err := workbench.VerifyWebAssets(); err != nil {
			return fail(err)
		}
		return 0
	}
	if args[0] == "guest-os" {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := writeGuestOSInspection(ctx, args[1:], os.Stdout); err != nil {
			return fail(err)
		}
		return 0
	}
	if args[0] == "verify-vm-assets" {
		if len(args) != 1 {
			return 2
		}
		if err := verifyVMAssets(os.MkdirTemp, func(dir string) error {
			_, err := limaembed.WriteShare(dir)
			return err
		}); err != nil {
			return fail(err)
		}
		return 0
	}
	path := os.Getenv(cage.ConfigEnv)
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".claude-cage", "config.json")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return fail(err)
	}
	if args[0] == "update" {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := runHostUpdate(ctx, path, updateOptions, os.Stdin, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, workbench.HostCLIDiagnostic(err.Error()))
			if ctx.Err() != nil {
				return 130
			}
			return 1
		}
		return 0
	}
	if args[0] == "interface-language" {
		if len(args) == 3 && args[1] == "--seed-windows-id" {
			id, err := strconv.Atoi(args[2])
			if err == nil {
				err = uilocale.SeedWindowsInstallerChoice(filepath.Dir(path), id)
			}
			if err != nil {
				return fail(err)
			}
			return 0
		}
		if len(args) != 2 || args[1] != "--json" {
			return 2
		}
		language, err := uilocale.Native(filepath.Dir(path))
		if err != nil {
			return fail(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(language); err != nil {
			return fail(err)
		}
		return 0
	}
	if err := host.ConfigureWindowsRuntime(); err != nil {
		return fail(err)
	}
	if args[0] == "install-cli" {
		if len(args) != 1 {
			return fail(errors.New("usage: cage install-cli"))
		}
		return installTerminalCommand()
	}
	if namedAccount || slices.Contains([]string{"claude", "account", "start", "up", "status", "restart", "login"}, args[0]) || args[0] == "stop" && len(args) == 1 {
		action := args[0]
		parameters := args[1:]
		if namedAccount {
			action, parameters = "run-by-name", args
		} else if action == "claude" {
			action = "run"
		} else if action != "account" && action != "start" && action != "login" && len(args) != 1 {
			return fail(fmt.Errorf("usage: cage %s", action))
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		code, err := workbench.HostCLI(ctx, path, action, parameters, os.Stdin, os.Stdout, os.Stderr)
		if err != nil {
			if ctx.Err() != nil {
				return 130
			}
			fmt.Fprintln(os.Stderr, workbench.HostCLIDiagnostic(err.Error()))
			return code
		}
		return code
	}
	// The controller boots or synchronizes its guest through this private
	// subprocess. The public "up" command only opens the native desktop.
	if args[0] == "vm-start" {
		args[0] = "up"
	}
	store := diagnostics.New(filepath.Dir(path))
	diagnostics.SetDefault(store)
	if args[0] == "container-runtime" {
		if len(args) != 2 {
			return fail(fmt.Errorf("container-runtime prepare|start|stop|inspect"))
		}
		environment, err := containerenv.Load(filepath.Dir(path))
		if err != nil {
			return fail(err)
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		switch args[1] {
		case "prepare":
			err = environment.Prepare(ctx, os.Stdout)
		case "start":
			err = environment.Start(ctx, path, os.Stdout)
		case "stop":
			err = environment.Stop(ctx)
		case "inspect":
			inspection, inspectErr := environment.Inspect(ctx)
			err = inspectErr
			if err == nil {
				err = json.NewEncoder(os.Stdout).Encode(inspection)
			}
		default:
			err = fmt.Errorf("container-runtime prepare|start|stop|inspect")
		}
		if err != nil {
			return fail(err)
		}
		return 0
	}
	if args[0] == "container-configure" {
		if len(args) != 2 || args[1] != "--json" {
			return fail(fmt.Errorf("container-configure --json < configuration.json"))
		}
		if err := configureContainer(path); err != nil {
			return fail(err)
		}
		return 0
	}
	if args[0] == "container-install-script" {
		if len(args) != 2 {
			return fail(fmt.Errorf("container-install-script tools|services"))
		}
		script, err := containerInstallScript(args[1])
		if err != nil {
			return fail(err)
		}
		_, err = io.WriteString(os.Stdout, script)
		if err != nil {
			return 1
		}
		return 0
	}
	if args[0] == "container-network" {
		if len(args) != 1 {
			return 2
		}
		environment, err := containerenv.Load(filepath.Dir(path))
		if err != nil {
			return fail(err)
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := environment.ServeNetwork(ctx, path); err != nil {
			return fail(err)
		}
		return 0
	}
	if args[0] == "container-tap" {
		if len(args) > 2 {
			return 2
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := containerenv.RunTAP(ctx, os.Stdin, os.Stdout, args[1:]...); err != nil {
			return fail(err)
		}
		return 0
	}
	if args[0] == "server-entry" {
		if len(args) != 2 || args[1] != "--json" {
			return fail(fmt.Errorf("server-entry --json"))
		}
		entry, err := workbench.ServerEntry(path)
		if err != nil {
			return fail(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(entry); err != nil {
			return 1
		}
		return 0
	}
	if args[0] == "diagnostic-event" {
		var event struct {
			Source  string `json:"source"`
			Message string `json:"message"`
		}
		if json.NewDecoder(io.LimitReader(os.Stdin, 32<<10)).Decode(&event) != nil {
			return 2
		}
		if event.Source != "native" {
			return 2
		}
		if err := store.Record("error", event.Source, event.Message); err != nil {
			return 1
		}
		return 0
	}
	if args[0] == "diagnostics" {
		connection := ""
		if len(args) == 5 && args[1] == "--json" && args[2] == "--connection" {
			connection = args[3]
			args = []string{args[0], args[1], args[4]}
		}
		if len(args) != 3 || args[1] != "--json" {
			return fail(fmt.Errorf("diagnostics --json /absolute/path.zip"))
		}
		if err := workbench.ExportDiagnosticsForConnection(workbench.DesktopHelperConfig(path, connection), connection, args[2]); err != nil {
			diagnostics.Log("error", "command", err.Error())
			_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "error", "message": err.Error()})
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "ok"})
		return 0
	}
	if args[0] == "log-relay" {
		if len(args) != 2 {
			return 2
		}
		return relay(path, args[1])
	}
	if args[0] == "app-update" {
		return appupdate.Run(path, args[1:], os.Stdout)
	}
	connectionID := ""
	if (args[0] == "clipboard-files" || args[0] == "upload-files" || args[0] == "browser-file" || args[0] == "desktop-file" || args[0] == "share-project") && len(args) >= 3 && args[2] == "--connection" {
		if len(args) < 4 || args[3] == "" {
			return fail(fmt.Errorf("missing file connection identity"))
		}
		connectionID = args[3]
		args = append(append([]string{}, args[:2]...), args[4:]...)
	}
	if args[0] == "clipboard-files" {
		if len(args) < 4 || args[1] != "--json" {
			return fail(fmt.Errorf("clipboard-files --json --connection ID send|receive PATH..."))
		}
		result, err := workbench.ClipboardFilesForConnection(workbench.DesktopHelperConfig(path, connectionID), connectionID, args[2], args[3:])
		if err != nil {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"message": err.Error()})
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(result)
		return 0
	}
	if args[0] == "desktop-file" {
		if len(args) != 3 || args[1] != "--json" {
			return fail(fmt.Errorf("desktop-file --json --connection ID REQUEST"))
		}
		result, err := workbench.DesktopFileForConnection(workbench.DesktopHelperConfig(path, connectionID), connectionID, args[2])
		if err != nil {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "error", "message": err.Error()})
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(result)
		return 0
	}
	if args[0] == "share-project" {
		if connectionID == "" || len(args) != 4 || args[1] != "--json" || (args[2] != "read" && args[2] != "write") {
			return fail(fmt.Errorf("share-project --json --connection ID read|write DIRECTORY"))
		}
		result, err := workbench.ShareNativeCloudProject(workbench.DesktopHelperConfig(path, connectionID), connectionID, args[3], args[2] == "read")
		if err != nil {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "error", "message": err.Error()})
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(result)
		return 0
	}
	if args[0] == "upload-files" {
		if len(args) < 5 || args[1] != "--json" {
			return fail(fmt.Errorf("upload-files --json browser|desktop|terminal ID PATH..."))
		}
		result, err := workbench.UploadHostFilesForConnection(workbench.DesktopHelperConfig(path, connectionID), connectionID, args[2], args[3], args[4:])
		if err != nil {
			diagnostics.Log("error", "command", err.Error())
			_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "error", "message": err.Error()})
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(result)
		return 0
	}
	if args[0] == "browser-file" {
		if len(args) < 4 || len(args) > 5 || args[1] != "--json" {
			return fail(fmt.Errorf("browser-file --json save|open|reveal ID [destination]"))
		}
		destination := ""
		if len(args) == 5 {
			destination = args[4]
		}
		result, err := workbench.BrowserFileForConnection(workbench.DesktopHelperConfig(path, connectionID), connectionID, args[2], args[3], destination)
		if err != nil {
			diagnostics.Log("error", "command", err.Error())
			_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "error", "message": err.Error()})
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(result)
		return 0
	}
	if args[0] == "serve-desktop" {
		if err := workbench.ServeDesktop(path); err != nil {
			return fail(err)
		}
		return 0
	}
	if args[0] == "serve" {
		if err := workbench.Serve(path); err != nil {
			return fail(err)
		}
		return 0
	}
	if args[0] == "desktop" {
		options := workbench.DesktopOptions{}
		returnJSON := false
		for _, argument := range args[1:] {
			switch argument {
			case "--json":
				returnJSON = true
			case "--complete-update":
				options.CompleteUpdate = true
			case "--existing":
				options.UseExisting = true
			default:
				return fail(fmt.Errorf("unknown desktop option: %s", argument))
			}
		}
		entry, err := workbench.DesktopEntry(path, options)
		if err != nil {
			if returnJSON {
				diagnostics.Log("error", "command", err.Error())
				_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "error", "message": err.Error()})
				return 1
			}
			return fail(err)
		}
		// Native launchers consume this on a pipe, never in a shared log.
		if returnJSON {
			_ = json.NewEncoder(os.Stdout).Encode(entry)
		} else {
			if entry.URL == "" {
				return fail(fmt.Errorf("%s", entry.Message))
			}
			if err := openWorkbench(entry.URL); err != nil {
				return fail(err)
			}
			fmt.Println("Cage workbench opened.")
		}
		return 0
	}
	if args[0] == "prepare-update" {
		stop, returnJSON := false, false
		for _, argument := range args[1:] {
			switch argument {
			case "--stop":
				stop = true
			case "--json":
				returnJSON = true
			default:
				return fail(fmt.Errorf("unknown prepare-update option: %s", argument))
			}
		}
		result, err := workbench.PrepareDesktopUpdate(path, stop)
		if err != nil {
			if returnJSON {
				diagnostics.Log("error", "command", err.Error())
				_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "error", "message": err.Error()})
				return 1
			}
			return fail(err)
		}
		if returnJSON {
			_ = json.NewEncoder(os.Stdout).Encode(result)
		} else if result.Status != "prepared" {
			return fail(fmt.Errorf("%s", result.Message))
		}
		return 0
	}
	if args[0] == "init" {
		if err = host.EnsurePrivateDir(filepath.Dir(path)); err != nil {
			return fail(err)
		}
		data, err := json.MarshalIndent(cage.DefaultConfig(), "", "  ")
		if err == nil {
			err = host.CreatePrivateFile(path, append(data, '\n'))
		}
		if err != nil {
			return fail(err)
		}
		fmt.Println("Configuration created:", path)
		return 0
	}
	// The workbench owns the proxy lifetime during internal environment
	// restarts. A normal CLI stop still shuts down both the guest and core.
	keepProxy := len(args) > 1 && args[0] == "stop" && args[1] == "--keep-proxy"
	if keepProxy {
		args = append([]string{"stop"}, args[2:]...)
	}
	cfg, err := cage.Load(path)
	management := args[0] == "stop" || args[0] == "list" || args[0] == "info"
	if err != nil && !management {
		return fail(err)
	}

	os.Setenv(cage.ConfigEnv, path)
	profile := filepath.Dir(path)
	state := filepath.Join(profile, "state")
	limaHome := filepath.Join(state, "lima")
	os.Unsetenv("CAGE_LIMA_CACHE_DIR")
	os.Unsetenv("CAGE_LOGICAL_LIMA_HOME")
	if !containerenv.Configured(profile) {
		if err = cage.RecoverStorage(context.Background(), profile, nil); err != nil {
			return fail(err)
		}
		lease, location, err := cage.AcquireStorageUse(profile)
		if err != nil {
			return fail(err)
		}
		defer lease.Close()
		// Keep image caches on the same volume the initialization page shows,
		// including the default location. The saved path survives app updates.
		os.Setenv("CAGE_LIMA_CACHE_DIR", filepath.Join(location.Directory, "cache", "lima"))
		limaHome, err = cage.ProfileLimaHome(profile, location.Custom)
		if err != nil {
			return fail(err)
		}
		if location.Custom {
			state = filepath.Dir(limaHome)
			os.Setenv("CAGE_LOGICAL_LIMA_HOME", limaHome)
		}
	}
	os.Setenv("AGENT_VM_STATE_DIR", state)
	os.Setenv("AGENT_VM_LIMA_HOME", limaHome)
	// Do not inherit host proxy settings or credentials into VM operations.
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "FTP_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "ftp_proxy", "no_proxy", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
		os.Unsetenv(name)
	}
	// Lima's host-side image downloader follows system proxy settings/TUN.
	// Guest traffic keeps the isolated hostagent hooks in limaembed.Run.
	http.DefaultTransport = hostproxy.NewTransport()
	if limaembed.Run(args) {
		return 0
	}
	if args[0] == "proxy-start" || args[0] == "proxy-stop" {
		if err := proxyCommand(args[0], filepath.Dir(path), cfg); err != nil {
			return fail(err)
		}
		return 0
	}
	if args[0] == "run" {
		// Before the switch to the environment directory below.
		here, hereErr := os.Getwd()
		if args, err = followRunDirectory(path, args, here, hereErr, os.Stderr, terminalConfirm(os.Stdin, os.Stderr)); err != nil {
			return fail(err)
		}
	}
	if handled, err := containerOperation(path, args, cfg, keepProxy); handled {
		if err != nil {
			return fail(err)
		}
		return 0
	}
	// Every Cage operation targets this profile's one persistent environment.
	switch args[0] {
	case "up", "login", "shell", "run", "claude", "stop", "doctor", "access", "name":
		directory := cage.EnvironmentDir(filepath.Dir(path))
		if err := os.MkdirAll(directory, 0700); err != nil {
			return fail(err)
		}
		if err := os.Chdir(directory); err != nil {
			return fail(err)
		}
	}
	if management {
		code := cli.Main(args, cli.Stdio())
		if code == 0 && args[0] == "stop" && !keepProxy {
			if err := managedproxy.Stop(filepath.Dir(path)); err != nil {
				return fail(err)
			}
		}
		return code
	}
	if err := cfg.RequireManagedProxy(filepath.Dir(path)); err != nil {
		return fail(err)
	}
	if err := cfg.CheckRunning(state); err != nil {
		return fail(err)
	}

	if args[0] == "check" {
		// Use the same fixed exit services as the network check, so one
		// blocked endpoint does not fail this check on its own.
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		err := checkProxyTransport(ctx, filepath.Dir(path), cfg)
		cancel()
		if err != nil {
			return fail(fmt.Errorf("proxy transport check failed (no direct fallback): %w", err))
		}
		fmt.Println("Proxy transport OK. Guest traffic still requires VM acceptance checks.")
		return 0
	}
	if args[0] == "setup" {
		if cfg.GuestProfile == nil {
			loc, e := cage.GetStorageLocation(filepath.Dir(path))
			if e != nil {
				return fail(e)
			}
			if !loc.HasEnvironment {
				cfg.GuestProfile, e = guestprofile.Create(filepath.Dir(path), guestprofile.Options{})
				if e != nil {
					return fail(e)
				}
			}
		}
		if err = os.MkdirAll(state, 0700); err != nil {
			return fail(err)
		}
		if err = os.WriteFile(filepath.Join(state, "setup.sh"), []byte(cfg.SetupScript()), 0600); err != nil {
			return fail(err)
		}
		memory, cpus, resourceErr := cage.ConfiguredResourceLimits(path)
		if resourceErr != nil {
			return fail(resourceErr)
		}
		disk, diskErr := cage.ConfiguredDiskLimit(path)
		if diskErr != nil {
			return fail(diskErr)
		}
		args = append([]string{"setup", "--memory", fmt.Sprint(memory), "--cpus", fmt.Sprint(cpus), "--disk", fmt.Sprint(disk), "--preinstall=node,python,chromium,gh,claude,code-server"}, requireWorkbenchTools(args[1:])...)
	}
	if args[0] == "up" {
		return startEnvironment(startArgs, cli.Stdio(), cli.Main, func() error {
			return workbench.StartGuestServices(context.Background(), path, os.Stdout, os.Stderr)
		})
	}
	if args[0] == "update-tools" {
		if result := cli.Main([]string{"run", "true"}, cli.Stdio()); result != 0 {
			return result
		}
		if err := workbench.RunMaintenanceScript(context.Background(), path, cage.ToolsUpdateScriptFor(os.Getenv(cage.InstallTLS12Env) == "1"), os.Stdout, os.Stderr); err != nil {
			return fail(err)
		}
		return 0
	}
	if args[0] == "access" {
		args = []string{"run", "bash", "-c", `printf 'Editor password: '; sed -n 's/^password: //p' "$HOME/.config/claude-cage/code-server.yaml"; printf 'Browser password: '; cat "$HOME/.config/claude-cage/browser-password"; printf '\n'`}
	}
	allowed := map[string]bool{"setup": true, "run": true, "shell": true, "claude": true, "stop": true, "list": true, "doctor": true, "info": true, "name": true}
	if !allowed[args[0]] {
		return fail(fmt.Errorf("unknown cage command %q; see cage help", args[0]))
	}
	if args[0] == "run" || args[0] == "shell" || args[0] == "claude" {
		warnClosedNetwork(path, os.Stderr)
	}
	return cli.Main(args, cli.Stdio())
}

func containerInstallScript(kind string) (string, error) {
	switch kind {
	case "services":
		return cage.GuestSetup, nil
	case "tools":
		script := "set -e\nexport CLAUDE_CAGE=1\n" + cage.InstallTLS12Prelude(os.Getenv(cage.InstallTLS12Env) == "1") + cage.APTBootstrapScript + "\n" + cage.ToolInstallPrelude()
		enabled := map[string]bool{"NODE": true, "PYTHON": true, "CHROMIUM": true, "GH": true, "CLAUDE": true, "CODE_SERVER": true}
		for _, name := range []string{"PYTHON", "NODE", "RUBY", "RUST", "GOLANG", "DOCKER", "CHROMIUM", "GH", "CLAUDE", "OPENCODE", "CODEX", "VIBE", "PI", "CODE_SERVER", "CODE_CLAUDE", "CODE_OPENCODE", "CODE_CODEX", "CODE_VIBE", "MCP_CHROME", "MCP_PLAYWRIGHT"} {
			value := "0"
			if enabled[name] {
				value = "1"
			}
			script += "export AGENT_VM_INSTALL_" + name + "=" + value + "\n"
		}
		return script + agentvm.SetupScript, nil
	default:
		return "", fmt.Errorf("container-install-script tools|services")
	}
}

// Service assets exceed Windows' command-line limit. Give only this internal
// invocation a script stream; interactive login and ordinary run keep stdin.
func runGuestServiceStart(script string, stdio cli.IO, invoke func([]string, cli.IO) int) int {
	stdio.Stdin = strings.NewReader(script)
	return invoke([]string{"run", "bash", "-s"}, stdio)
}

func startEnvironment(args []string, stdio cli.IO, invoke func([]string, cli.IO) int, startServices func() error) int {
	if result := invoke(args, stdio); result != 0 {
		return result
	}
	if err := startServices(); err != nil {
		return fail(err)
	}
	fmt.Fprintln(stdio.Stdout, "Cage local services are ready. External networking requires workbench verification.")
	return 0
}

// The shared environment's start script needs these tools. A --preinstall
// list given to `cage setup`, such as "default", keeps its choices and gains
// any of them it lacks, so the base it builds can always start.
var workbenchTools = []string{"chromium", "code-server"}

func requireWorkbenchTools(args []string) []string {
	complete := func(list string) string {
		if list == "" {
			list = "default"
		}
		names := strings.Split(list, ",")
		for _, tool := range workbenchTools {
			if !slices.Contains(names, tool) {
				names = append(names, tool)
			}
		}
		return strings.Join(names, ",")
	}
	result := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case strings.HasPrefix(a, "--preinstall="):
			result = append(result, "--preinstall="+complete(strings.TrimPrefix(a, "--preinstall=")))
		case a == "--preinstall":
			// As in setup, only a following non-option is its value.
			value := ""
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				value = args[i]
			}
			result = append(result, "--preinstall="+complete(value))
		default:
			result = append(result, a)
		}
	}
	return result
}

// Commands whose remaining arguments belong to a program in the environment,
// such as `cage run ls -h`, or that answer --help themselves.
var helpPassthrough = map[string]bool{
	"agent": true, "run": true, "shell": true, "sh": true, "claude": true, "code": true,
	"opencode": true, "codex": true, "vibe": true, "pi": true,
}

// subcommandHelp answers `cage COMMAND --help` before the command opens or
// creates a profile, so asking for help never runs it. It prints the
// command's lines from the overview, or the overview when it has none.
func subcommandHelp(args []string) (string, bool) {
	if len(args) < 2 || args[1] != "--help" && args[1] != "-h" || helpPassthrough[args[0]] {
		return "", false
	}
	if len(args) != 2 && slices.Contains([]string{"start", "up", "login", "update"}, args[0]) {
		return "", false
	}
	var lines []string
	matched := false
	for _, line := range strings.Split(help, "\n") {
		switch {
		case strings.HasPrefix(line, "cage "):
			fields := strings.Fields(line)
			matched = len(fields) > 1 && fields[1] == args[0]
		case !strings.HasPrefix(line, " "):
			matched = false
		}
		if matched {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return help, true
	}
	return strings.Join(lines, "\n") + "\n\nRun 'cage --help' for all commands.\n", true
}

func fail(err error) int {
	diagnostics.Log("error", "command", err.Error())
	fmt.Fprintln(os.Stderr, strings.TrimSpace(err.Error()))
	return 1
}

func openWorkbench(address string) error {
	if runtime.GOOS == "windows" {
		return exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", address).Start()
	}
	command := "xdg-open"
	if runtime.GOOS == "darwin" {
		command = "open"
	}
	return exec.Command(command, address).Run()
}

// Relay owns stdout/stderr capture outside the child so a Go runtime panic or
// abrupt child exit is recorded without ever persisting raw output.
func relay(configPath, kind string) int {
	var cmd *exec.Cmd
	source := "controller"
	if kind == "controller" {
		exe, err := os.Executable()
		if err != nil {
			return fail(err)
		}
		cmd = exec.Command(exe, "serve")
	} else if kind == "proxy" {
		core, err := managedproxy.CorePath()
		if err != nil {
			return fail(err)
		}
		profile := filepath.Dir(configPath)
		runtimePath := filepath.Join(managedproxy.Dir(profile), "config.yaml")
		if _, err := managedproxy.BoundInterface(runtimePath); err != nil {
			return fail(err)
		}
		cmd = exec.Command(core, "-d", managedproxy.Dir(profile), "-f", runtimePath)
		source = "proxy"
	} else {
		return 2
	}
	configureRelayCommand(cmd)
	cmd.Env = relayEnv(kind, configPath, os.Environ())
	store := diagnostics.New(filepath.Dir(configPath))
	out, stderr := store.Stream(source, "info"), store.Stream(source, "error")
	if kind == "proxy" {
		out, stderr = managedproxy.CoreLogStream(store, filepath.Dir(configPath), "info"), managedproxy.CoreLogStream(store, filepath.Dir(configPath), "error")
	}
	defer out.Close()
	defer stderr.Close()
	cmd.Stdout, cmd.Stderr = out, stderr
	if err := cmd.Start(); err != nil {
		return fail(err)
	}
	diagnostics.Log("info", source, "process started")
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	done := make(chan struct{})
	var wg sync.WaitGroup
	var forwarded os.Signal
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case sig := <-signals:
			forwarded = sig
			_ = cmd.Process.Signal(sig)
		case <-done:
		}
	}()
	err := cmd.Wait()
	close(done)
	wg.Wait()
	if err != nil {
		if expectedRelayStop(err, forwarded) {
			diagnostics.Log("info", source, "process stopped on request")
			return 0
		}
		diagnostics.Log("error", source, "process exited: "+err.Error())
		return 1
	}
	diagnostics.Log("info", source, "process exited normally")
	return 0
}

// The relay runs before the host proxy variables are cleared for VM
// operations, so the proxy core would otherwise inherit them.
func relayEnv(kind, configPath string, env []string) []string {
	if kind == "proxy" {
		env = managedproxy.WithoutHostProxyEnv(env)
	}
	return append(env, cage.ConfigEnv+"="+configPath)
}

func expectedRelayStop(err error, forwarded os.Signal) bool {
	signal, ok := forwarded.(syscall.Signal)
	if !ok {
		return false
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == signal || exit.ExitCode() == 128+int(signal)
}

func verifyVMAssets(mktemp func(string, string) (string, error), writeShare func(string) error) error {
	dir, err := mktemp("", "cage-vm-assets-")
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("%w (ensure TEMP/TMP points to a writable directory; on Windows, executables built in a Low Mandatory Level / sandboxed checkout directory cannot write to %%TEMP%% — move the repository to a normal local directory such as C:\\src\\Claude-Cage)", err)
		}
		return err
	}
	defer os.RemoveAll(dir)
	return writeShare(dir)
}
