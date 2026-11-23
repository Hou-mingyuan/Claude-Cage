package hostbridge

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// PluginArchiveLimit is the largest compressed plugin archive accepted for sync.
const PluginArchiveLimit = marketArchiveLimit

// ValidPluginName reports whether name is a valid plugin directory identifier.
func ValidPluginName(name string) bool {
	return pluginName.MatchString(name) && len(name) <= 40
}

// SupportsPlatform reports whether a manifest can run on a host running goos.
// Guest plugins always run inside the Linux environment regardless of host OS.
func SupportsPlatform(m Manifest, goos string) bool {
	if m.Kind == KindGuest {
		return true
	}
	return len(m.Platforms) == 0 || slices.Contains(m.Platforms, goos)
}

// RequiresGUI reports whether a host plugin declares screen or input control,
// which requires an interactive graphical desktop session.
func RequiresGUI(m Manifest) bool {
	return m.Kind != KindGuest && (slices.Contains(m.Permissions, "screen") || slices.Contains(m.Permissions, "input"))
}

// CompareVersions compares two X.Y.Z semantic versions (ignoring prerelease
// suffixes, matching EngineSatisfied). It returns (-1, 0, 1) and whether both
// strings parsed as valid numeric versions.
func CompareVersions(a, b string) (int, bool) {
	va, okA := parseCoreVersion(a)
	vb, okB := parseCoreVersion(b)
	if !okA || !okB {
		return 0, false
	}
	return slices.Compare(va[:], vb[:]), true
}

func parseCoreVersion(raw string) ([3]int, bool) {
	var out [3]int
	core, _, _ := strings.Cut(strings.TrimSpace(raw), "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i := range out {
		if parts[i] == "" {
			return out, false
		}
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// ArchivePlugin deterministically packs the plugin in dir into a .tar.gz
// archive rooted at "<name>/", rejecting symlinks and oversized archives.
// It returns the compressed archive and the SHA-256 hex digest of its content.
func ArchivePlugin(dir, name string) ([]byte, string, error) {
	if !ValidPluginName(name) {
		return nil, "", errors.New("invalid plugin name")
	}
	manifest, err := LoadManifest(dir)
	if err != nil {
		return nil, "", err
	}
	if manifest.Name != name {
		return nil, "", fmt.Errorf("manifest name %q does not match %q", manifest.Name, name)
	}
	info, err := os.Lstat(dir)
	if err != nil || info.Mode().Type() != os.ModeDir {
		return nil, "", errors.New("plugin directory is not a plain folder")
	}
	type entry struct {
		rel   string
		full  string
		isDir bool
		mode  int64
		size  int64
	}
	var entries []entry
	var total int64
	files := 0
	err = filepath.WalkDir(dir, func(current string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, current)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		if strings.Contains(relSlash, "\\") || slices.Contains(strings.Split(relSlash, "/"), "..") {
			return fmt.Errorf("unexpected path %q", relSlash)
		}
		linfo, err := os.Lstat(current)
		if err != nil {
			return err
		}
		switch linfo.Mode().Type() {
		case os.ModeDir:
			entries = append(entries, entry{rel: relSlash, full: current, isDir: true, mode: 0o755})
		case 0:
			files++
			total += linfo.Size()
			if files > marketFileLimit || linfo.Size() < 0 || total > marketUnpackedLimit {
				return errors.New("the plugin archive is too large")
			}
			mode := int64(0o644)
			if linfo.Mode().Perm()&0o111 != 0 || filepath.Separator == '\\' {
				mode = 0o755
			}
			entries = append(entries, entry{rel: relSlash, full: current, mode: mode, size: linfo.Size()})
		default:
			return fmt.Errorf("%s is not a plain file or folder", relSlash)
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })

	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	contentHash := sha256.New()
	if err := tw.WriteHeader(&tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		return nil, "", err
	}
	for _, item := range entries {
		if item.isDir {
			if err := tw.WriteHeader(&tar.Header{Name: name + "/" + item.rel + "/", Typeflag: tar.TypeDir, Mode: item.mode}); err != nil {
				return nil, "", err
			}
			_, _ = contentHash.Write([]byte("D\x00" + item.rel + "\x00\n"))
			continue
		}
		data, err := os.ReadFile(item.full)
		if err != nil {
			return nil, "", err
		}
		if int64(len(data)) != item.size {
			return nil, "", fmt.Errorf("%s changed while packing", item.rel)
		}
		if err := tw.WriteHeader(&tar.Header{Name: name + "/" + item.rel, Typeflag: tar.TypeReg, Mode: item.mode, Size: int64(len(data))}); err != nil {
			return nil, "", err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, "", err
		}
		// Hash paths and file bytes independent of host executable bits so
		// Windows, macOS and Linux compute identical digests for the same plugin.
		_, _ = fmt.Fprintf(contentHash, "F\x00%s\x00%d\x00", item.rel, len(data))
		_, _ = contentHash.Write(data)
		_, _ = contentHash.Write([]byte{'\n'})
	}
	if err := tw.Close(); err != nil {
		return nil, "", err
	}
	digest := contentHash.Sum(nil)

	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(tarBuf.Bytes()); err != nil {
		return nil, "", err
	}
	if err := gw.Close(); err != nil {
		return nil, "", err
	}
	if int64(gzBuf.Len()) > marketArchiveLimit {
		return nil, "", errors.New("the compressed plugin archive is too large")
	}
	return gzBuf.Bytes(), hex.EncodeToString(digest), nil
}

// PluginDigest returns the deterministic SHA-256 hex digest of a plugin folder.
func PluginDigest(dir, name string) (string, error) {
	_, digest, err := ArchivePlugin(dir, name)
	return digest, err
}

// InstallArchive unpacks a .tar.gz plugin archive produced by ArchivePlugin into
// dir/<expectedName> after validating its manifest, platform support and engine
// compatibility. It never overwrites an existing target folder.
func InstallArchive(data []byte, expectedName, dir, appVersion string) (Manifest, error) {
	if !ValidPluginName(expectedName) {
		return Manifest{}, errors.New("invalid plugin name")
	}
	if len(data) == 0 || int64(len(data)) > marketArchiveLimit {
		return Manifest{}, errors.New("the plugin archive is empty or too large")
	}
	target := filepath.Join(dir, expectedName)
	if _, err := os.Lstat(target); err == nil {
		return Manifest{}, fmt.Errorf("plugin %s is already installed", expectedName)
	} else if !os.IsNotExist(err) {
		return Manifest{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Manifest{}, err
	}
	staging, err := os.MkdirTemp(dir, ".syncing-*")
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(staging)
	if err := unpackPlugin(data, expectedName, staging); err != nil {
		return Manifest{}, err
	}
	manifest, err := LoadManifest(filepath.Join(staging, expectedName))
	if err != nil {
		return Manifest{}, err
	}
	if manifest.Name != expectedName {
		return Manifest{}, errors.New("the plugin manifest name does not match")
	}
	if !manifest.Supported() {
		return Manifest{}, errors.New("the plugin does not support this system")
	}
	if unavailable := manifest.Unavailable(appVersion, true); unavailable != "" {
		return Manifest{}, fmt.Errorf("the plugin is not compatible with this version (%s)", unavailable)
	}
	if err := os.Rename(filepath.Join(staging, expectedName), target); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}
