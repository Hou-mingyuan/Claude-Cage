// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package qemu

import (
	"errors"
	"fmt"
	"os/exec"

	"golang.org/x/sys/windows"
)

// Some loader failures occur before QEMU can write to stderr. Preserve the
// cause and give the hostagent log a marker the workbench can surface.
func qemuExecutionError(err error) error {
	const blocked = "Windows 应用程序控制或签名校验阻止加载 QEMU。请使用有可信签名的 Cage 安装包"
	const missing = "QEMU 缺少运行时 DLL。请使用完整的 Cage 安装包"
	if errors.Is(err, windows.ERROR_ACCESS_DISABLED_BY_POLICY) || errors.Is(err, windows.ERROR_INVALID_IMAGE_HASH) {
		return fmt.Errorf("qemu[loader]: %s: %w", blocked, err)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		switch windows.NTStatus(uint32(exit.ExitCode())) {
		case windows.STATUS_INVALID_IMAGE_HASH, windows.STATUS_ACCESS_DISABLED_BY_POLICY_DEFAULT,
			windows.STATUS_ACCESS_DISABLED_BY_POLICY_PATH, windows.STATUS_ACCESS_DISABLED_BY_POLICY_PUBLISHER,
			windows.STATUS_ACCESS_DISABLED_BY_POLICY_OTHER:
			return fmt.Errorf("qemu[loader]: %s: %w", blocked, err)
		case windows.STATUS_DLL_NOT_FOUND:
			return fmt.Errorf("qemu[loader]: %s: %w", missing, err)
		}
	}
	return err
}
