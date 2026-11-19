// Package catalog selects verifiable release assets for native application updates.
package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"golang.org/x/mod/semver"
)

const Repository = "Lee-xeo/Claude-Cage"
const ReleasesURL = "https://api.github.com/repos/" + Repository + "/releases?per_page=100"

// Releases must stay below the limit of every published App that still updates
// in place; scripts/prepare-release-assets.mjs enforces that smaller limit.
const MaxArchive = 2 << 30

type Asset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
	State  string `json:"state"`
}
type Release struct {
	Tag        string  `json:"tag_name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"assets"`
}
type Candidate struct {
	Version   string `json:"version"`
	Archive   Asset  `json:"archive"`
	Checksums Asset  `json:"checksums"`
}

func archiveName(version, goos, goarch string) (string, error) {
	switch {
	case goos == "darwin" && goarch == "arm64":
		return "Cage-" + version + "-macos-arm64.dmg", nil
	case goos == "windows" && goarch == "amd64":
		return "Cage-" + version + "-windows-amd64-setup.exe", nil
	// Linux only learns that a newer package exists; the system installs it.
	case goos == "linux" && goarch == "amd64":
		return "Cage-" + version + "-linux-amd64.deb", nil
	case goos == "android" && goarch == "arm64":
		return "Cage-" + version + "-android-arm64.apk", nil
	default:
		return "", errors.New("此平台尚无 Cage 应用内更新发行包")
	}
}

func Select(releases []Release, current, goos, goarch string) (*Candidate, error) {
	if _, err := archiveName(current, goos, goarch); err != nil {
		return nil, err
	}
	currentTag := "v" + current
	if !semver.IsValid(currentTag) || semver.Canonical(currentTag) != currentTag {
		return nil, errors.New("当前应用版本无效，无法安全比较更新")
	}
	var best *Candidate
	for _, r := range releases {
		if r.Draft || !strings.HasPrefix(r.Tag, "v") || !semver.IsValid(r.Tag) || semver.Canonical(r.Tag) != r.Tag || semver.Compare(r.Tag, currentTag) <= 0 {
			continue
		}
		pre := r.Prerelease || semver.Prerelease(r.Tag) != ""
		if semver.Prerelease(currentTag) == "" && pre {
			continue
		}
		v := strings.TrimPrefix(r.Tag, "v")
		if best != nil && semver.Compare(r.Tag, "v"+best.Version) <= 0 {
			continue
		}
		c := Candidate{Version: v}
		name, _ := archiveName(v, goos, goarch)
		for _, a := range r.Assets {
			if a.State != "uploaded" {
				continue
			}
			if a.Name == name && a.Size > 0 && a.Size <= MaxArchive {
				c.Archive = a
			}
			if a.Name == "SHA256SUMS" && a.Size > 0 && a.Size <= 64<<10 {
				c.Checksums = a
			}
		}
		if c.Archive.URL == "" {
			continue
		}
		base := "https://github.com/" + Repository + "/releases/download/" + r.Tag + "/"
		if c.Archive.URL != base+name || (c.Checksums.URL != "" && c.Checksums.URL != base+"SHA256SUMS") {
			continue
		}
		if c.Archive.Digest == "" && c.Checksums.URL == "" {
			continue
		}
		best = &c
	}
	return best, nil
}

func DigestValue(raw string) (string, error) {
	if !strings.HasPrefix(raw, "sha256:") {
		return "", errors.New("发行包缺少有效的 SHA-256 摘要")
	}
	h := strings.TrimPrefix(raw, "sha256:")
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != sha256.Size {
		return "", errors.New("发行包 SHA-256 摘要无效")
	}
	return strings.ToLower(h), nil
}

func (c Candidate) SHA256(b []byte) (string, error) {
	var hash string
	if c.Archive.Digest != "" {
		var err error
		hash, err = DigestValue(c.Archive.Digest)
		if err != nil {
			return "", err
		}
	}
	if c.Checksums.URL == "" {
		if hash == "" {
			return "", errors.New("发行包缺少校验信息")
		}
		return hash, nil
	}
	found := ""
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != c.Archive.Name {
			continue
		}
		h, err := DigestValue("sha256:" + fields[0])
		if err != nil || found != "" {
			return "", errors.New("发行包校验清单无效或重复")
		}
		found = h
	}
	if found == "" || (hash != "" && hash != found) {
		return "", errors.New("发行包与校验清单的 SHA-256 不一致")
	}
	return found, nil
}
