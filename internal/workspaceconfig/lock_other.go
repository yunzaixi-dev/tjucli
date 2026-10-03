//go:build !linux && !darwin && !windows

package workspaceconfig

import (
	"errors"
	"os"
)

const configLockSupported = false

func lockConfigFile(*os.File) error {
	return errors.New("workspace config locking is unsupported on this platform")
}

func unlockConfigFile(*os.File) {}
