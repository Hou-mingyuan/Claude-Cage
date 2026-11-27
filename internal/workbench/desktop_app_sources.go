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
		appID: "codex",
		digests: []string{
			"47d6c55653f1108aebe49ee675e18ee5f41256dada662323395664cf3ef932b9", // 0.1.0
			"3904bea2b5496794389fc337290997f17b49e48a8dc56eb8dda6421a760e7f32", // 0.1.1
		},
		httpsHosts: []string{"persistent.oaistatic.com"},
	},
	"antigravity-app": {
		appID: "antigravity",
		digests: []string{
			"6bb9f74d083e7cf6051491f5f4c16d7004d1b2af85759197d1cb32047a4a2d1e", // 0.1.0
			"bb98aab794de583f07cf55eaaa4dafbbc3c11652e78e15e4830202fdcae502d3", // 0.1.1
			"ef97c92feeed39e70b9ae3319aafa1ac0c36b6e4efe599df204288ebd4a10a6c", // 0.1.2
		},
		httpsHosts: []string{"antigravity-hub-auto-updater-974169037036.us-central1.run.app", "storage.googleapis.com"},
	},
}

type desktopAppInstallPlan struct {
	script           string
	httpsHosts       []string
	allowedAPTAppID  string
	unreviewedPlugin string
}

func prepareDesktopAppInstall(t desktopAppTarget, review desktopAppReview) (desktopAppInstallPlan, error) {
	var plan desktopAppInstallPlan
	if review.appID == t.app.ID && review.appID != "" {
		archive, digest, err := hostbridge.ArchivePlugin(t.dir, t.plugin)
		if err == nil && slices.Contains(review.digests, digest) {
			// The installed files may be updated while the operation is queued.
			// Take every installer input, including the resolver, from the verified bytes.
			root, err := os.MkdirTemp("", "desktop-install-*")
			if err != nil {
				return plan, err
			}
			defer os.RemoveAll(root)
			manifest, err := hostbridge.InstallArchive(archive, t.plugin, root, version.Version)
			if err != nil {
				return plan, err
			}
			if manifest.Kind != hostbridge.KindGuest || manifest.Contributes == nil {
				return plan, errors.New("插件未提供已审核的桌面应用")
			}
			idx := slices.IndexFunc(manifest.Contributes.DesktopApps, func(app hostbridge.DesktopApp) bool { return app.ID == review.appID })
			if idx < 0 {
				return plan, errors.New("插件未提供已审核的桌面应用")
			}
			t.app = manifest.Contributes.DesktopApps[idx]
			t.dir = filepath.Join(root, t.plugin)
			plan.httpsHosts = review.httpsHosts
		} else {
			plan.unreviewedPlugin = t.plugin
		}
	}
	if t.app.Source.Type == hostbridge.SourceApt {
		plan.allowedAPTAppID = t.id
	}
	script, err := desktopAppInstallScript(t)
	if err != nil {
		return plan, err
	}
	plan.script = script
	return plan, nil
}
