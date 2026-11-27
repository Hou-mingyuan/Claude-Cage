//go:build !darwin && !windows && !linux

package main

import (
	"context"
	"errors"
	"os"

	"github.com/sylvinus/agent-vm/internal/appupdate"
)

func installHostUpdate(context.Context, string, appupdate.State, *os.File, *os.File, *os.File, bool) error {
	return errors.New("请通过此平台的原安装渠道更新 Cage。")
}
