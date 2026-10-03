package workspaceruntime

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/yunzaixi-dev/tjucli/internal/workspaceconfig"
)

// privateBytes secures an empty exclusive file before writing, then publishes
// atomically in its verified parent. Use for auth snapshots as well as configs:
// chmod alone is not a privacy boundary on Windows.
func privateBytes(path string, data []byte) error {
	parent := filepath.Dir(path)
	if workspaceconfig.CheckPrivateDir(parent) != nil {
		return ErrInvalidConfig
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return ErrInvalidConfig
	}
	defer root.Close()
	name := filepath.Base(path)
	if info, err := root.Lstat(name); err == nil {
		if !info.Mode().IsRegular() {
			return ErrInvalidConfig
		}
	} else if !os.IsNotExist(err) {
		return ErrInvalidConfig
	}
	id, err := randomID()
	if err != nil {
		return err
	}
	temp := ".write-" + id
	f, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return ErrInvalidConfig
	}
	defer root.Remove(temp)
	// Shared helper checks symlinks/reparse points and native ownership.
	err = workspaceconfig.EnsurePrivateFile(filepath.Join(parent, temp))
	if err == nil {
		opened, statErr := f.Stat()
		current, pathErr := root.Lstat(temp)
		if statErr != nil || pathErr != nil || !os.SameFile(opened, current) {
			err = ErrInvalidConfig
		}
	}
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ErrInvalidConfig
	}
	// Open the directory before publication: an approval consumer can remove
	// the request directory immediately after seeing response.json.
	dir, err := root.Open(".")
	if err != nil {
		return ErrInvalidConfig
	}
	defer dir.Close()
	if root.Rename(temp, name) != nil || syncPrivateDirectory(dir) != nil {
		return ErrInvalidConfig
	}
	return nil
}

// Windows does not support os.File.Sync on directory handles. Match the shared
// config writer: synced regular files and atomic replacement, no claim of
// Unix-equivalent directory fsync durability on Windows.
func syncPrivateDirectory(dir *os.File) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	return dir.Sync()
}

func syncPrivateParent(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return ErrInvalidConfig
	}
	defer dir.Close()
	if syncPrivateDirectory(dir) != nil {
		return ErrInvalidConfig
	}
	return nil
}
