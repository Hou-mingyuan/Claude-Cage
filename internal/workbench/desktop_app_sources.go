package workbench

import (
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/sylvinus/agent-vm/internal/hostbridge"
	"github.com/sylvinus/agent-vm/internal/version"
)

// These are ArchivePlugin content digests (paths and uncompressed file bytes).
// Review the market package before adding its digest and exact HTTPS origins.
type desktopAppReview struct {
	appID      string
	digests    []string
	httpsHosts []string
}

var reviewedDesktopApps = map[string]desktopAppReview{
	"codex-app": {
		appID:      "codex",
		digests:    []string{"3904bea2b5496794389fc337290997f17b49e48a8dc56eb8dda6421a760e7f32"}, // 0.1.1
		httpsHosts: []string{"persistent.oaistatic.com"},
	},
	"antigravity-app": {
		appID: "antigravity",
		digests: []string{
			"bb98aab794de583f07cf55eaaa4dafbbc3c11652e78e15e4830202fdcae502d3", // 0.1.1
			"ef97c92feeed39e70b9ae3319aafa1ac0c36b6e4efe599df204288ebd4a10a6c", // 0.1.2
		},
		httpsHosts: []string{"antigravity-hub-auto-updater-974169037036.us-central1.run.app", "storage.googleapis.com"},
	},
}

func prepareDesktopAppInstall(t desktopAppTarget, review desktopAppReview) (string, []string, error) {
	var hosts []string
	if review.appID == t.app.ID && review.appID != "" {
		archive, digest, err := hostbridge.ArchivePlugin(t.dir, t.plugin)
		if err == nil && slices.Contains(review.digests, digest) {
			// The installed files may be updated while the operation is queued.
			// Take every installer input, including the resolver, from the verified bytes.
			root, err := os.MkdirTemp("", "desktop-install-*")
			if err != nil {
				return "", nil, err
			}
			defer os.RemoveAll(root)
			manifest, err := hostbridge.InstallArchive(archive, t.plugin, root, version.Version)
			if err != nil {
				return "", nil, err
			}
			if manifest.Kind != hostbridge.KindGuest || manifest.Contributes == nil {
				return "", nil, errors.New("插件未提供已审核的桌面应用")
			}
			idx := slices.IndexFunc(manifest.Contributes.DesktopApps, func(app hostbridge.DesktopApp) bool { return app.ID == review.appID })
			if idx < 0 {
				return "", nil, errors.New("插件未提供已审核的桌面应用")
			}
			t.app = manifest.Contributes.DesktopApps[idx]
			t.dir = filepath.Join(root, t.plugin)
			hosts = review.httpsHosts
		}
	}
	script, err := desktopAppInstallScript(t)
	return script, hosts, err
}
