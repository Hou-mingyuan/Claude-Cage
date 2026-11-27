package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sylvinus/agent-vm/internal/appupdate"
	"github.com/sylvinus/agent-vm/internal/appupdate/catalog"
	"golang.org/x/mod/semver"
	"golang.org/x/sys/unix"
)

const linuxPackageCLI = "/usr/lib/claude-cage/cage"
const linuxUpdateHelper = "/usr/lib/claude-cage/install-update"

func installHostUpdate(ctx context.Context, config string, release appupdate.State, stdin, stdout, stderr *os.File, assumeYes bool) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if err := checkLinuxInstallation(ctx, executable); err != nil {
		return err
	}
	pkg, err := appupdate.FetchLinuxPackage(ctx, config, release, hostUpdateProgress(stderr))
	if err != nil {
		return err
	}
	archive, err := openLinuxUpdate(ctx, pkg, release.LatestVersion)
	if err != nil {
		return err
	}
	defer archive.Close()
	prompt := fmt.Sprintf("已校验 Cage %s 的系统安装包。安装前请保存任务、停止环境并退出工作台。是否使用 APT 升级？", pkg.LatestVersion)
	if err := confirmHostUpdate(ctx, stdin, stderr, prompt, assumeYes); err != nil {
		return err
	}
	args := []string{linuxUpdateHelper, strings.Replace(pkg.LatestVersion, "-", "~", 1), pkg.SHA256}
	if os.Geteuid() != 0 {
		if err := unix.Access("/usr/bin/sudo", unix.X_OK); err != nil {
			return errors.New("系统未提供 sudo。请让管理员通过 APT 安装官方 Cage .deb。")
		}
		args = append([]string{"/usr/bin/sudo", "--"}, args...)
	}
	return execLinuxUpdate(ctx, archive, args)
}

func checkLinuxInstallation(ctx context.Context, executable string) error {
	if executable != linuxPackageCLI {
		if strings.HasSuffix(executable, "/cloud-host/bin/cage") || strings.HasSuffix(executable, "/.local/lib/cage/cage") {
			return errors.New("此 Cage 来自云端部署。请通过原服务器部署流程更新云端服务，并保留配置目录和容器数据。")
		}
		return errors.New("此 Cage 来自独立二进制或源码构建。请通过原安装渠道或源码构建流程升级。")
	}
	running, err := os.Stat("/proc/self/exe")
	if err != nil {
		return err
	}
	dir, err := os.Lstat(filepath.Dir(linuxPackageCLI))
	if err != nil || !dir.IsDir() || !rootOwnsLinuxUpdateFile(dir) {
		return errors.New("Cage 系统安装目录的归属或权限异常。请通过 APT 重新安装官方 Cage .deb。")
	}
	for _, path := range []string{linuxPackageCLI, linuxUpdateHelper} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || !rootOwnsLinuxUpdateFile(info) {
			return errors.New("Cage 系统安装文件缺失或权限异常。请通过 APT 重新安装官方 Cage .deb。")
		}
		if path == linuxPackageCLI && !os.SameFile(running, info) {
			return errors.New("此 Cage 进程启动后系统安装已变化。请重新运行 cage update。")
		}
	}
	// Consult the system database independently of DPKG_ADMINDIR and other
	// per-process package settings. The distro's /usr/bin/cage is unrelated.
	const db = "/usr/bin/dpkg-query"
	cmd := exec.CommandContext(ctx, db, "--show", "--showformat=${Status}\t${Architecture}", "claude-cage")
	cmd.Env = []string{"LC_ALL=C"}
	status, err := cmd.Output()
	if string(status) == "hold ok installed\tamd64" {
		return errors.New("claude-cage 已被系统包管理器锁定版本。请联系管理员确认升级。")
	}
	if err != nil || string(status) != "install ok installed\tamd64" {
		return errors.New("系统未登记完整安装的 claude-cage amd64 软件包。请使用原安装渠道维护。")
	}
	cmd = exec.CommandContext(ctx, db, "--search", linuxPackageCLI, linuxUpdateHelper)
	cmd.Env = []string{"LC_ALL=C"}
	owners, err := cmd.Output()
	expected := "claude-cage: " + linuxPackageCLI + "\nclaude-cage: " + linuxUpdateHelper + "\n"
	if err != nil || string(owners) != expected {
		return errors.New("dpkg 无法确认当前 Cage 和更新工具属于 claude-cage 软件包。请使用原安装渠道维护。")
	}
	return nil
}

func rootOwnsLinuxUpdateFile(info os.FileInfo) bool {
	return info.Sys().(*syscall.Stat_t).Uid == 0 && info.Mode().Perm()&0022 == 0
}

// Keep the opened file through confirmation and sudo. The installer hashes its
// own copy again so writes to the cache cannot change what APT receives.
func openLinuxUpdate(ctx context.Context, pkg appupdate.State, expected string) (*os.File, error) {
	tag := "v" + expected
	if pkg.LatestVersion != expected || !semver.IsValid(tag) || semver.Canonical(tag) != tag {
		return nil, errors.New("待安装的版本与此次检查不一致，请重新运行 cage update。")
	}
	digest, err := catalog.DigestValue("sha256:" + pkg.SHA256)
	if err != nil || digest != pkg.SHA256 || !filepath.IsAbs(pkg.ArchivePath) {
		return nil, errors.New("发行包路径或 SHA-256 摘要无效。")
	}
	file, err := os.OpenFile(pkg.ArchivePath, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	valid := false
	defer func() {
		if !valid {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > catalog.MaxArchive || info.Size() != pkg.TotalBytes {
		return nil, errors.New("下载的系统安装包类型或大小与发行信息不一致。")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.NewSectionReader(file, 0, info.Size()+1))
	if err != nil || n != info.Size() || hex.EncodeToString(hash.Sum(nil)) != digest {
		return nil, errors.New("下载的系统安装包与官方 SHA-256 摘要不一致。")
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/dpkg-deb", "--show", "--showformat=${Package}\t${Version}\t${Architecture}", "/proc/self/fd/3")
	cmd.ExtraFiles = []*os.File{file}
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	data, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil || string(data) != "claude-cage\t"+strings.Replace(expected, "-", "~", 1)+"\tamd64" {
		return nil, errors.New("下载的 .deb 包名、版本或架构与本次 Cage 更新不一致。")
	}
	valid = true
	return file, nil
}

// Replacing the CLI lets the existing dpkg guard see every OTHER packaged
// process, including another user's service. sudo reads passwords from the TTY.
func execLinuxUpdate(ctx context.Context, archive *os.File, args []string) error {
	saved, err := unix.FcntlInt(os.Stdin.Fd(), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		return err
	}
	defer unix.Close(saved)
	source := int(archive.Fd())
	if source == 0 {
		// A service may close standard input before running cage update --yes.
		// In that case open allocated fd 0 for the archive with CLOEXEC set.
		source = saved
	}
	if err = unix.Dup3(source, 0, 0); err != nil {
		return err
	}
	defer unix.Dup3(saved, 0, 0)
	if err := ctx.Err(); err != nil {
		return err
	}
	env := []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8", "TERM=" + os.Getenv("TERM")}
	if err := unix.Exec(args[0], args, env); err != nil {
		return fmt.Errorf("无法启动系统软件包安装程序：%w", err)
	}
	return nil
}
