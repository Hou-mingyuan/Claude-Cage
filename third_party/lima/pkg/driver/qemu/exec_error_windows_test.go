// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package qemu

import (
	"errors"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestQemuExecutionErrorWindows(t *testing.T) {
	for _, cause := range []error{windows.ERROR_ACCESS_DISABLED_BY_POLICY, windows.ERROR_INVALID_IMAGE_HASH} {
		startErr := &os.PathError{Op: "fork/exec", Path: "qemu-system-x86_64.exe", Err: cause}
		if err := qemuExecutionError(startErr); !errors.Is(err, cause) || !strings.Contains(err.Error(), "qemu[loader]: Windows 应用程序控制") {
			t.Errorf("lost policy error: %v", err)
		}
	}
	if err := qemuExecutionError(os.ErrNotExist); err != os.ErrNotExist {
		t.Errorf("unrelated failure was mislabeled: %v", err)
	}
}
