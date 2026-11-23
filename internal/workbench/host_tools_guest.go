package workbench

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sylvinus/agent-vm/internal/hostbridge"
)

// guestPluginRoot is where plugin files are kept in the environment, under
// the user's home there. The name says nothing about this application.
const guestPluginRoot = ".local/share/plugins"

const (
	cagePluginName            = "cage-plugin"
	cagePluginGuestFolderName = "plugin-studio"
)

// guestPluginFolder maps a host plugin identifier to a neutral folder name
// inside the workspace environment so product identifiers never appear in
// guest paths or working directories.
func guestPluginFolder(name string) string {
	if name == cagePluginName {
		return cagePluginGuestFolderName
	}
	if _, found := hostbridge.CheckWording(&mcp.Tool{Name: name}); found {
		sum := sha256.Sum256([]byte(name))
		return "plugin-" + hex.EncodeToString(sum[:6])
	}
	return name
}

// guestPluginLimit is the most a plugin's files may add up to.
const guestPluginLimit = 100 << 20

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'" }

// packPlugin is the plugin's files as a tar archive and its digest. Only
// plain files and folders go in, as when a plugin is installed. The manifest
// and UI i18n file stay behind: they are read on the host and may mention
// this application, which nothing in the environment needs to be told.
func packPlugin(dir string) ([]byte, string, error) {
	return packPluginWithExtra(dir, nil)
}

func packPluginWithExtra(dir string, extra map[string][]byte) ([]byte, string, error) {
	var buffer bytes.Buffer
	archive := tar.NewWriter(&buffer)
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if relative == hostbridge.ManifestFile || relative == hostbridge.I18nFile {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		header := &tar.Header{Name: filepath.ToSlash(relative), Mode: 0o644}
		switch {
		case info.IsDir():
			header.Typeflag, header.Mode = tar.TypeDir, 0o755
			header.Name += "/"
			return archive.WriteHeader(header)
		case !info.Mode().IsRegular():
			return fmt.Errorf("%s is not a plain file", relative)
		}
		// Windows keeps no executable bit; there every file may be a program.
		if info.Mode().Perm()&0o111 != 0 || filepath.Separator == '\\' {
			header.Mode = 0o755
		}
		if int64(buffer.Len())+info.Size() > guestPluginLimit {
			return errors.New("the plugin is too large")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		header.Typeflag, header.Size = tar.TypeReg, int64(len(data))
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		_, err = archive.Write(data)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	if len(extra) > 0 {
		names := make([]string, 0, len(extra))
		for name := range extra {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			data := extra[name]
			if int64(buffer.Len()+len(data)) > guestPluginLimit {
				return nil, "", errors.New("the plugin is too large")
			}
			header := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data))}
			if err := archive.WriteHeader(header); err != nil {
				return nil, "", err
			}
			if _, err := archive.Write(data); err != nil {
				return nil, "", err
			}
		}
	}
	if err := archive.Close(); err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(buffer.Bytes())
	return buffer.Bytes(), hex.EncodeToString(digest[:]), nil
}

// guestPluginSyncScript replaces the plugin's folder with the archive on its
// standard input, whole or not at all. $1 is the folder, $2 the digest.
const guestPluginSyncScript = `set -e
dir="$HOME/$1"
rm -rf "$dir.new"
mkdir -p "$dir.new"
tar -xf - -C "$dir.new"
printf '%s' "$2" > "$dir.new/.digest"
rm -rf "$dir"
mv "$dir.new" "$dir"`

// guestPluginRunScript runs the plugin's program in its folder. $1 is the
// folder, $2 is 1 when the program gets the variables of the user's
// settings, the rest the program and its arguments. The program is taken
// from the folder when it is there, as on this computer, and from PATH
// otherwise. One still running from before, as when it does not stop when
// its input closes, is ended first: it is known by the folder it runs in.
// The program gets the environment of an ordinary session, not the
// variables the environment's own services set for themselves. The file of
// settings is read once and removed, so values stay only in the program.
const guestPluginRunScript = `dir="$HOME/$1"
settings=$2
shift 2
note="$dir.pid"
if [ -f "$note" ]; then
  old=$(cat "$note")
  case "$(readlink "/proc/$old/cwd" 2>/dev/null)" in "$dir"|"$dir (deleted)") kill "$old" 2>/dev/null ;; esac
fi
cd "$dir" || exit 1
echo $$ > "$note"
program=$1
shift
case "$program" in /*) ;; *) [ -f "./$program" ] && program="./$program" ;; esac
set -- "$program" "$@"
if [ "$settings" = 1 ] && [ -f "$dir.env" ]; then . "$dir.env"; fi
rm -f "$dir.env"
exec env -i HOME="$HOME" USER="$(id -un)" LOGNAME="$(id -un)" SHELL="${SHELL:-/bin/sh}" PATH="$PATH" LANG="${LANG:-C.UTF-8}" ${LC_ALL:+LC_ALL="$LC_ALL"} ${TZ:+TZ="$TZ"} "$@"`

// guestPluginSettingsScript writes, from its standard input, the file the
// run script reads the settings from. Only the user can read it.
const guestPluginSettingsScript = `umask 077
mkdir -p "$(dirname "$HOME/$1")"
cat > "$HOME/$1.env"`

// guestPluginSettingsFile prepends the settings to the arguments of env, as
// NAME=value words, so the run script needs no parsing of its own.
func guestPluginSettingsFile(env []string) string {
	line := "set --"
	for _, entry := range env {
		line += " " + shellQuote(entry)
	}
	return line + ` "$@"` + "\n"
}

type guestMarketCatalogEntry struct {
	Name             string                              `json:"name"`
	DisplayName      string                              `json:"display_name,omitempty"`
	Version          string                              `json:"version,omitempty"`
	Description      string                              `json:"description,omitempty"`
	I18n             map[string]hostbridge.LocalizedText `json:"i18n,omitempty"`
	Categories       []string                            `json:"categories,omitempty"`
	Permissions      []string                            `json:"permissions"`
	Target           string                              `json:"target"`
	Installed        bool                                `json:"installed"`
	InstalledVersion string                              `json:"installed_version,omitempty"`
	Supported        bool                                `json:"supported"`
	HasView          bool                                `json:"has_view,omitempty"`
	HasMCP           bool                                `json:"has_mcp,omitempty"`
}

func neutralCatalogText(value string) string {
	if value == "" {
		return ""
	}
	if _, found := hostbridge.CheckWording(&mcp.Tool{Name: "t", Description: value}); found {
		return ""
	}
	return value
}

func neutralCatalogI18n(raw map[string]hostbridge.LocalizedText) map[string]hostbridge.LocalizedText {
	if len(raw) == 0 {
		return nil
	}
	clean := make(map[string]hostbridge.LocalizedText, len(raw))
	for lang, item := range raw {
		dn := neutralCatalogText(item.DisplayName)
		desc := neutralCatalogText(item.Description)
		if dn == "" && desc == "" {
			continue
		}
		clean[lang] = hostbridge.LocalizedText{DisplayName: dn, Description: desc}
	}
	if len(clean) == 0 {
		return nil
	}
	return clean
}

// guestMarketCatalogJSON builds a sanitized, neutral snapshot of available
// market plugins and installed plugins so cage-plugin's search_plugins tool
// can check the market before scaffolding a new plugin, without exposing
// product or environment identifiers inside the workspace environment.
func (s *Server) guestMarketCatalogJSON(ctx context.Context) []byte {
	s.hostMarket.mu.Lock()
	entries := append([]hostbridge.MarketEntry(nil), s.hostMarket.entries...)
	fetched := s.hostMarket.fetched
	s.hostMarket.mu.Unlock()
	if len(entries) == 0 && ctx != nil {
		fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if fresh, ts, err := s.hostMarketEntries(fetchCtx, false); err == nil && len(fresh) > 0 {
			entries, fetched = fresh, ts
		}
		cancel()
	}

	installed := map[string]hostPluginView{}
	for _, p := range s.hostToolsView().Plugins {
		if p.BuiltIn {
			continue
		}
		installed[p.Name] = p
	}

	byName := map[string]guestMarketCatalogEntry{}
	for _, entry := range entries {
		if entry.Name == cagePluginName || entry.Name == workspaceControlPlugin {
			continue
		}
		if _, found := hostbridge.CheckWording(&mcp.Tool{Name: entry.Name}); found {
			continue
		}
		target := hostbridge.DraftTargetWorkspace
		if entry.Kind != hostbridge.KindGuest {
			target = hostbridge.DraftTargetLocal
		}
		hasView := entry.Contributes != nil && len(entry.Contributes.Views) > 0
		hasMCP := entry.Contributes == nil || len(entry.Contributes.MCPServers) > 0
		perms := entry.Permissions
		if perms == nil {
			perms = []string{}
		}
		item := guestMarketCatalogEntry{
			Name:        entry.Name,
			DisplayName: neutralCatalogText(entry.DisplayName),
			Version:     entry.Version,
			Description: neutralCatalogText(entry.Description),
			I18n:        neutralCatalogI18n(entry.I18n),
			Categories:  hostbridge.ShownCategories(entry.Categories),
			Permissions: perms,
			Target:      target,
			Supported:   entry.Supported(),
			HasView:     hasView,
			HasMCP:      hasMCP,
		}
		if inst, ok := installed[entry.Name]; ok {
			item.Installed = true
			item.InstalledVersion = inst.Version
		}
		byName[entry.Name] = item
	}
	for name, inst := range installed {
		if _, seen := byName[name]; seen || name == cagePluginName || name == workspaceControlPlugin {
			continue
		}
		if _, found := hostbridge.CheckWording(&mcp.Tool{Name: name}); found {
			continue
		}
		target := hostbridge.DraftTargetWorkspace
		if inst.Kind != hostbridge.KindGuest {
			target = hostbridge.DraftTargetLocal
		}
		hasView := inst.Contributes != nil && len(inst.Contributes.Views) > 0
		hasMCP := inst.Contributes == nil || len(inst.Contributes.MCPServers) > 0
		perms := inst.Permissions
		if perms == nil {
			perms = []string{}
		}
		byName[name] = guestMarketCatalogEntry{
			Name:             name,
			DisplayName:      neutralCatalogText(inst.DisplayName),
			Version:          inst.Version,
			Description:      neutralCatalogText(inst.Description),
			I18n:             neutralCatalogI18n(inst.I18n),
			Categories:       inst.Categories,
			Permissions:      perms,
			Target:           target,
			Installed:        true,
			InstalledVersion: inst.Version,
			Supported:        inst.Supported,
			HasView:          hasView,
			HasMCP:           hasMCP,
		}
	}

	list := make([]guestMarketCatalogEntry, 0, len(byName))
	for _, item := range byName {
		list = append(list, item)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })

	updatedAt := ""
	if !fetched.IsZero() {
		updatedAt = fetched.UTC().Format(time.RFC3339)
	}
	payload, _ := json.MarshalIndent(map[string]any{
		"updated_at": updatedAt,
		"plugins":    list,
	}, "", "  ")
	return append(payload, '\n')
}

const guestWriteCatalogScript = `set -e
dir="$HOME/$1"
[ -d "$dir" ] || exit 0
cat > "$dir/market-catalog.json.tmp"
mv "$dir/market-catalog.json.tmp" "$dir/market-catalog.json"`

func (s *Server) refreshGuestMarketCatalogAsync() {
	s.mu.Lock()
	ready, closing := s.ready, s.closing
	s.mu.Unlock()
	if !ready || closing || s.clientOnly {
		return
	}
	if _, err := os.Lstat(filepath.Join(s.hostPluginDir(), cagePluginName)); err != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(s.lifetimeContext(), 10*time.Second)
		defer cancel()
		data := s.guestMarketCatalogJSON(ctx)
		folder := guestPluginRoot + "/" + guestPluginFolder(cagePluginName)
		cmd := s.sshCommand(ctx, "-T", s.sshTarget(), "--", "sh -c "+shellQuote(guestWriteCatalogScript)+" sh "+shellQuote(folder))
		cmd.Stdin = bytes.NewReader(data)
		_ = cmd.Run()
	}()
}

// guestPluginCommand copies the plugin's files into the environment, unless
// they are there already, and returns the command that runs its server
// there, with its standard input and output carried over SSH.
func (s *Server) guestPluginCommand(ctx context.Context, manifest hostbridge.Manifest, dir string) (*exec.Cmd, error) {
	command, err := hostbridge.ExpandCommand(manifest.ServerCommand(), s.guestActiveProjectDir())
	if err != nil {
		return nil, err
	}
	if len(command) == 0 {
		return nil, errors.New("the plugin has no MCP server")
	}
	env, err := s.pluginSettingsEnv(ctx, manifest)
	if err != nil {
		return nil, err
	}
	var extra map[string][]byte
	if manifest.Name == cagePluginName {
		extra = map[string][]byte{"market-catalog.json": s.guestMarketCatalogJSON(ctx)}
	}
	archive, digest, err := packPluginWithExtra(dir, extra)
	if err != nil {
		return nil, err
	}
	folder := guestPluginRoot + "/" + guestPluginFolder(manifest.Name)
	prepare, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	current, _ := s.sshCommand(prepare, "-T", s.sshTarget(), "--", "cat "+shellQuote(folder+"/.digest")+" 2>/dev/null").Output()
	if strings.TrimSpace(string(current)) != digest {
		sync := s.sshCommand(prepare, "-T", s.sshTarget(), "--", "sh -c "+shellQuote(guestPluginSyncScript)+" sh "+shellQuote(folder)+" "+shellQuote(digest))
		sync.Stdin = bytes.NewReader(archive)
		if output, err := sync.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("copying the plugin: %w: %s", err, strings.TrimSpace(string(output)))
		}
	}
	settings := "0"
	if len(env) > 0 {
		write := s.sshCommand(prepare, "-T", s.sshTarget(), "--", "sh -c "+shellQuote(guestPluginSettingsScript)+" sh "+shellQuote(folder))
		// Values travel on standard input, never on a command line.
		write.Stdin = strings.NewReader(guestPluginSettingsFile(env))
		if output, err := write.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("writing the plugin's settings: %w: %s", err, strings.TrimSpace(string(output)))
		}
		settings = "1"
	}
	line := "sh -c " + shellQuote(guestPluginRunScript) + " sh " + shellQuote(folder) + " " + settings
	for _, argument := range command {
		line += " " + shellQuote(argument)
	}
	// Not tied to ctx, which ends with the call that starts the program: the
	// plugin's own Close ends it.
	return s.sshCommand(s.lifetimeContext(), "-T", s.sshTarget(), "--", line), nil
}
