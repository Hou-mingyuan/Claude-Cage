package cagemobile

import (
	"encoding/json"
	"errors"

	"github.com/sylvinus/agent-vm/internal/appupdate/catalog"
)

func AndroidReleasesURL() string { return catalog.ReleasesURL }

type AndroidUpdate struct{ candidate *catalog.Candidate }

func SelectAndroidUpdate(current string, metadata []byte) (*AndroidUpdate, error) {
	var releases []catalog.Release
	if err := json.Unmarshal(metadata, &releases); err != nil || releases == nil {
		return nil, errors.New("更新源返回了无效的版本信息")
	}
	c, err := catalog.Select(releases, current, "android", "arm64")
	if err != nil || c == nil {
		return nil, err
	}
	return &AndroidUpdate{candidate: c}, nil
}

func (u *AndroidUpdate) Version() string      { return u.candidate.Version }
func (u *AndroidUpdate) DownloadURL() string  { return u.candidate.Archive.URL }
func (u *AndroidUpdate) Size() int64          { return u.candidate.Archive.Size }
func (u *AndroidUpdate) ChecksumsURL() string { return u.candidate.Checksums.URL }
func (u *AndroidUpdate) ExpectedDigest(manifest []byte) (string, error) {
	return u.candidate.SHA256(manifest)
}
