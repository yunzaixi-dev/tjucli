//go:build windows

package workspaceruntime

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/yunzaixi-dev/tjucli/internal/workspaceconfig"
)

var (
	sessionKernel32 = syscall.NewLazyDLL("kernel32.dll")
	sessionLockEx   = sessionKernel32.NewProc("LockFileEx")
	sessionUnlockEx = sessionKernel32.NewProc("UnlockFileEx")
)

// A synchronous nonblocking exclusive one-byte lock beyond EOF, matching the
// Unix try-lock semantics. LockFileEx documents FAIL_IMMEDIATELY and supports
// ranges beyond EOF; closing the handle also releases any outstanding lock.
// https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-lockfileex
func lockSession(ctx context.Context, path string) (func(), error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, ErrCanceled
	}
	if workspaceconfig.CheckPrivateDir(filepath.Dir(path)) != nil {
		return nil, ErrInvalidConfig
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, ErrInvalidConfig
	}
	defer root.Close()
	name := filepath.Base(path)
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	created := err == nil
	if os.IsExist(err) {
		if workspaceconfig.CheckPrivateFile(path) != nil {
			return nil, ErrInvalidConfig
		}
		f, err = root.OpenFile(name, os.O_RDWR, 0)
	}
	if err != nil {
		return nil, ErrInvalidConfig
	}
	fail := func(err error) (func(), error) {
		f.Close()
		return nil, err
	}
	if created && workspaceconfig.EnsurePrivateFile(path) != nil {
		return fail(ErrInvalidConfig)
	}
	info, statErr := f.Stat()
	current, pathErr := root.Lstat(name)
	if statErr != nil || pathErr != nil || !info.Mode().IsRegular() ||
		!os.SameFile(info, current) || workspaceconfig.CheckPrivateFile(path) != nil {
		return fail(ErrInvalidConfig)
	}
	if sessionLockEx.Find() != nil || sessionUnlockEx.Find() != nil {
		return fail(ErrRuntimeUnavailable)
	}
	var offset syscall.Overlapped
	ok, _, nativeErr := sessionLockEx.Call(f.Fd(), 3, /* EXCLUSIVE | FAIL_IMMEDIATELY */
		0, 1, 0, uintptr(unsafe.Pointer(&offset)))
	runtime.KeepAlive(f)
	if ok == 0 {
		if nativeErr == syscall.Errno(33) { // ERROR_LOCK_VIOLATION
			return fail(ErrSessionBusy)
		}
		return fail(ErrInvalidConfig)
	}
	unlock := func() {
		var offset syscall.Overlapped
		_, _, _ = sessionUnlockEx.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&offset)))
		runtime.KeepAlive(f)
		_ = f.Close()
	}
	if ctx.Err() != nil {
		unlock()
		return nil, ErrCanceled
	}
	return unlock, nil
}
