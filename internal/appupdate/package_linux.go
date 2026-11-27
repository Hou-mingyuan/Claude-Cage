package appupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/sylvinus/agent-vm/internal/host"
	"golang.org/x/sys/unix"
)

// FetchLinuxPackage returns the verified official .deb to the package install
// command. The caller must independently bind those same bytes to the privileged
// install. Neither the release URL nor the destination comes from command flags.
func FetchLinuxPackage(ctx context.Context, config string, state State, report func(State)) (State, error) {
	release := state.selected
	if release == nil || release.Version != state.LatestVersion || state.Phase != "available" {
		return state, errors.New("待下载的软件包与此次更新检查不符，请重新运行 cage update。")
	}
	dir := filepath.Join(filepath.Dir(config), "updates")
	if err := host.EnsurePrivateDir(dir); err != nil {
		return state, err
	}
	lock, err := os.Open(dir)
	if err != nil {
		return state, err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return state, errors.New("同一配置的 Cage 更新正在下载，请稍后重试。")
		}
		return state, err
	}
	client := updateClient(config)
	defer client.CloseIdleConnections()
	return fetchArchive(ctx, client, config, *release, "Cage.deb", state, report)
}
