//go:build !linux && !darwin && !windows

package managedproxy

import "errors"

func preflightProcessStart(int) (uint64, string, error) {
	return 0, "", errors.New("unsupported platform")
}

func stopPreflightProcess(preflightCoreRecord, string) error { return nil }
