// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package hostagent

import (
	"context"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/lima-vm/lima/v2/pkg/driver"
	"github.com/lima-vm/lima/v2/pkg/limatype"
	"github.com/lima-vm/lima/v2/pkg/sshutil"
)

// ManageResources is an optional embedding hook. It starts after the driver and
// stops before it. Guest observations use the existing private SSH transport.
var ManageResources func(context.Context, string, *limatype.LimaYAML, driver.MemoryBalloon, func(context.Context, string) ([]byte, error)) func()

func (a *HostAgent) startResourceManager(ctx context.Context) func() {
	if ManageResources == nil {
		return func() {}
	}
	d := a.driver
	if configured, ok := d.(*driver.ConfiguredDriver); ok {
		d = configured.Driver
	}
	balloon, ok := d.(driver.MemoryBalloon)
	if !ok {
		return func() {}
	}
	session := &resourceProbeSession{command: a.resourceProbeCommand}
	stop := ManageResources(ctx, a.instDir, a.instConfig, balloon, session.read)
	var once sync.Once
	return func() { once.Do(func() { stop(); session.close() }) }
}

// Retain a bounded prefix while draining all stderr, so a noisy SSH process
// cannot block. The embedding application's diagnostic store redacts the error.
type resourceProbeStderr struct{ data []byte }

func (b *resourceProbeStderr) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 4096 - len(b.data); remaining > 0 {
		b.data = append(b.data, p[:min(remaining, n)]...)
	}
	return n, nil
}

func (a *HostAgent) resourceProbeCommand(ctx context.Context, script string) *exec.Cmd {
	args := a.sshConfig.Args()
	if runtime.GOOS == "windows" {
		// The bundled Windows SSH client cannot run the probe over Lima's mux socket.
		args = sshutil.DisableControlMasterOptsFromSSHArgs(args)
	}
	args = append(args, "-o", "ConnectTimeout=2", "-o", "ClearAllForwardings=yes", "-T", "-p", strconv.Itoa(a.sshLocalPort), a.instSSHAddress, "--", script)
	cmd := exec.CommandContext(ctx, a.sshConfig.Binary(), args...)
	hideResourceProbe(cmd)
	cmd.WaitDelay = time.Second
	return cmd
}
