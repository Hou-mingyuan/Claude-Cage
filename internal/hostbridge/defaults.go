package hostbridge

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// defaultPluginFiles holds the official plugins installed by default on first
// setup. Unlike built-in plugins (such as commands), users can disable or
// uninstall them freely; uninstalling records dismissal so updates never force
// them back.
//
//go:embed defaults
var defaultPluginFiles embed.FS

// DefaultPlugin describes one preinstalled plugin bundled with the application.
type DefaultPlugin struct {
	Manifest Manifest
	I18n     map[string]LocalizedText
	Digest   string
}

// DefaultPlugins lists every valid default plugin bundled with the application,
// sorted by plugin name.
func DefaultPlugins() []DefaultPlugin {
	entries, err := defaultPluginFiles.ReadDir("defaults")
	if err != nil {
		return nil
	}
	var list []DefaultPlugin
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		plugin, ok := DefaultPluginByName(entry.Name())
		if ok {
			list = append(list, plugin)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Manifest.Name < list[j].Manifest.Name })
	return list
}

// DefaultPluginByName returns the bundled default plugin with the given name.
func DefaultPluginByName(name string) (DefaultPlugin, bool) {
	if !ValidPluginName(name) {
		return DefaultPlugin{}, false
	}
	root := path.Join("defaults", name)
	data, err := defaultPluginFiles.ReadFile(path.Join(root, ManifestFile))
	if err != nil {
		return DefaultPlugin{}, false
	}
	manifest, err := parseManifest(data)
	if err != nil || manifest.Name != name {
		return DefaultPlugin{}, false
	}
	digest, err := defaultPluginDigest(root)
	if err != nil {
		return DefaultPlugin{}, false
	}
	return DefaultPlugin{
		Manifest: manifest,
		I18n:     loadEmbedI18n(path.Join(root, I18nFile)),
		Digest:   digest,
	}, true
}

// InstallDefaultPlugin atomically copies the named default plugin into
// dir/<name> and returns its deterministic content digest. It never overwrites
// an existing target directory.
func InstallDefaultPlugin(name, dir string) (string, error) {
	plugin, ok := DefaultPluginByName(name)
	if !ok {
		return "", errors.New("unknown default plugin")
	}
	target := filepath.Join(dir, name)
	if _, err := os.Lstat(target); err == nil {
		return "", fmt.Errorf("plugin %s is already installed", name)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(dir, ".installing-default-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)

	source := path.Join("defaults", name)
	err = fs.WalkDir(defaultPluginFiles, source, func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel := strings.TrimPrefix(file, source)
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			return nil
		}
		dst := filepath.Join(staging, filepath.FromSlash(rel))
		if entry.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		raw, err := defaultPluginFiles.ReadFile(file)
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o644)
		if strings.HasSuffix(rel, ".py") || strings.HasSuffix(rel, ".sh") || strings.HasSuffix(rel, ".js") {
			mode = 0o755
		}
		return os.WriteFile(dst, raw, mode)
	})
	if err != nil {
		return "", err
	}
	if err := os.Rename(staging, target); err != nil {
		return "", err
	}
	return plugin.Digest, nil
}

// defaultPluginDigest computes the exact same cross-platform deterministic
// content hash as ArchivePlugin / PluginDigest so a deployed folder can be
// compared against the bundled default to detect user edits.
func defaultPluginDigest(root string) (string, error) {
	type item struct {
		rel   string
		full  string
		isDir bool
	}
	var items []item
	err := fs.WalkDir(defaultPluginFiles, root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || current == root {
			return walkErr
		}
		rel := strings.TrimPrefix(current, root+"/")
		items = append(items, item{rel: rel, full: current, isDir: entry.IsDir()})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].rel < items[j].rel })
	h := sha256.New()
	for _, entry := range items {
		if entry.isDir {
			_, _ = h.Write([]byte("D\x00" + entry.rel + "\x00\n"))
			continue
		}
		raw, err := defaultPluginFiles.ReadFile(entry.full)
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(h, "F\x00%s\x00%d\x00", entry.rel, len(raw))
		_, _ = h.Write(raw)
		_, _ = h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func loadEmbedI18n(file string) map[string]LocalizedText {
	raw, err := defaultPluginFiles.ReadFile(file)
	if err != nil {
		return nil
	}
	var parsed map[string]LocalizedText
	if json.Unmarshal(raw, &parsed) != nil || len(parsed) == 0 {
		return nil
	}
	cleaned := make(map[string]LocalizedText, len(parsed))
	for lang, entry := range parsed {
		lang = strings.TrimSpace(lang)
		desc := strings.TrimSpace(entry.Description)
		if lang == "" || desc == "" {
			continue
		}
		cleaned[lang] = LocalizedText{
			DisplayName: strings.TrimSpace(entry.DisplayName),
			Description: desc,
		}
	}
	if len(cleaned) == 0 {
		return nil
	}
	return cleaned
}
