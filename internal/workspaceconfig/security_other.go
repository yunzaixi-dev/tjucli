//go:build !windows

package workspaceconfig

import (
	"errors"
	"os"
)

func ensurePrivatePermissions(f *os.File, directory bool) error {
	return f.Chmod(privateMode(directory))
}

func checkPrivatePermissions(f *os.File, directory bool) error {
	info, err := f.Stat()
	if err != nil {
		return errors.New("inspect private permissions failed")
	}
	if info.Mode().Perm() != privateMode(directory) {
		if directory {
			return errors.New("workspace directory permissions must be 0700")
		}
		return errors.New("workspace config permissions must be 0600")
	}
	return nil
}

func prepareExistingPrivateLock(*os.File) error { return nil }

func privateFilePathSafe(string) bool { return true }
