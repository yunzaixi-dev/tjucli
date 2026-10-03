//go:build unix

package workspaceruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/yunzaixi-dev/tjucli/internal/workspaceconfig"
)

func lockSession(ctx context.Context, path string) (func(), error) {
	if workspaceconfig.CheckPrivateDir(filepath.Dir(path)) != nil {
		return nil, ErrInvalidConfig
	}
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, ErrInvalidConfig
	}
	f := os.NewFile(uintptr(fd), path)
	info, statErr := f.Stat()
	current, pathErr := os.Lstat(path)
	if statErr != nil || pathErr != nil || !info.Mode().IsRegular() ||
		!os.SameFile(info, current) || workspaceconfig.CheckPrivateFile(path) != nil {
		f.Close()
		return nil, ErrInvalidConfig
	}
	if ctx.Err() != nil {
		f.Close()
		return nil, ErrCanceled
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrSessionBusy
		}
		return nil, ErrInvalidConfig
	}
	return func() { _ = syscall.Flock(fd, syscall.LOCK_UN); _ = f.Close() }, nil
}
