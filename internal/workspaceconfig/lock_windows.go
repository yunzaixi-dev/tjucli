//go:build windows

package workspaceconfig

import (
	"os"
	"syscall"
	"unsafe"
)

const configLockSupported = true

var (
	configKernel32 = syscall.NewLazyDLL("kernel32.dll")
	configLockEx   = configKernel32.NewProc("LockFileEx")
	configUnlockEx = configKernel32.NewProc("UnlockFileEx")
)

func lockConfigFile(f *os.File) error {
	if err := configLockEx.Find(); err != nil {
		return err
	}
	// os.OpenFile supplies a synchronous handle. Without FAIL_IMMEDIATELY,
	// LockFileEx waits for the exclusive one-byte range at offset zero.
	// Windows permits a lock beyond EOF, so the persistent file stays empty.
	var offset syscall.Overlapped
	ok, _, err := configLockEx.Call(f.Fd(), 2 /* LOCKFILE_EXCLUSIVE_LOCK */, 0, 1, 0, uintptr(unsafe.Pointer(&offset)))
	if ok == 0 {
		if err != syscall.Errno(0) {
			return err
		}
		return syscall.EINVAL
	}
	return nil
}

func unlockConfigFile(f *os.File) {
	var offset syscall.Overlapped
	_, _, _ = configUnlockEx.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&offset)))
	// Closing the handle is the final release, including any unlock failure.
}
