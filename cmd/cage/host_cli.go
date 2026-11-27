package main

import (
	"errors"

	"github.com/sylvinus/agent-vm/internal/cagecmd"
)

// Only explicit Claude and named-account invocations may forward arguments
// to the guest. The account name stays separate from its Claude arguments.
func cageArgs(args []string) (command []string, named bool, err error) {
	if len(args) == 0 {
		return []string{"help"}, false, nil
	}
	if cagecmd.IsCommand(args[0]) {
		return args, false, nil
	}
	if args[0] == "--debug" {
		for _, word := range args {
			if word != "--debug" {
				if word == "hostagent" {
					return args, false, nil
				}
				break
			}
		}
	}
	if !cagecmd.ValidAccountName(args[0]) {
		return nil, false, errors.New("使用默认账号请运行 cage claude [参数]，指定账号请运行 cage <名称> [参数]。运行 cage 查看帮助。")
	}
	return args, true, nil
}
