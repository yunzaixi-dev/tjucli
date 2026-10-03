//go:build linux || darwin

package workspaceconfig

import (
	"errors"
	"os"
	"syscall"
)

const configLockSupported = true

func lockConfigFile(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

func unlockConfigFile(f *os.File) {
	// Closing the handle also releases the advisory lock, even if this call
	// fails. Independent opens serialize goroutines as well as processes.
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
