package workspaceconfig

import (
	"errors"
	"fmt"
	"os"
	"runtime"
)

const lockFileName = ".config.lock"

// The lock file is persistent, empty, regular and private. Removing it on
// unlock would let another writer lock a new inode while a waiter still uses
// the old inode. Only the native lock and its handle are released.
func withConfigLock(dir string, create bool, action func(*os.Root) error) error {
	if !configLockSupported {
		return errors.New("workspace config locking is unsupported on this platform")
	}
	r, err := openPrivateDir(dir, create)
	if err != nil {
		return err
	}
	defer r.Close()
	f, err := openLockFile(r)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := lockConfigFile(f); err != nil {
		return fmt.Errorf("lock workspace config: %w", err)
	}
	defer unlockConfigFile(f)
	// A blocked waiter must not continue if somebody replaced the lock path
	// while it waited; all cooperative writers must lock the same file.
	if err := checkLockFile(r, f); err != nil {
		return err
	}
	return action(r)
}

func openLockFile(r *os.Root) (*os.File, error) {
	// Exclusive creation avoids opening an attacker-controlled existing path
	// when establishing the lock for the first time.
	f, err := r.OpenFile(lockFileName, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err == nil {
		if err := ensurePrivatePermissions(f, false); err != nil {
			f.Close()
			return nil, fmt.Errorf("secure workspace config lock: %w", err)
		}
	} else {
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create workspace config lock: %w", err)
		}
		info, err := r.Lstat(lockFileName)
		if err != nil || !privateLockInfo(info) {
			return nil, errors.New("workspace config lock must be an empty private regular file, not a symlink")
		}
		f, err = r.OpenFile(lockFileName, os.O_RDWR|safeReadFlags, 0)
		if err != nil {
			return nil, fmt.Errorf("open workspace config lock: %w", err)
		}
		if err := checkLockIdentity(r, f); err != nil {
			f.Close()
			return nil, err
		}
		// On Windows a concurrent creator may not yet have protected an
		// owner-only inherited DACL. Only that already-private ACL may be
		// finalized; a permissive or foreign-owner lock is never repaired.
		if err := prepareExistingPrivateLock(f); err != nil {
			f.Close()
			return nil, err
		}
	}
	if err := checkLockFile(r, f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func privateLockInfo(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && info.Size() == 0 &&
		(runtime.GOOS == "windows" || info.Mode().Perm() == 0600)
}

func checkLockFile(r *os.Root, f *os.File) error {
	if err := checkLockIdentity(r, f); err != nil {
		return err
	}
	return checkPrivatePermissions(f, false)
}

func checkLockIdentity(r *os.Root, f *os.File) error {
	pathInfo, err := r.Lstat(lockFileName)
	if err != nil || !privateLockInfo(pathInfo) {
		return errors.New("workspace config lock path is unsafe or changed")
	}
	openInfo, err := f.Stat()
	if err != nil || !privateLockInfo(openInfo) || !os.SameFile(pathInfo, openInfo) {
		return errors.New("workspace config lock changed while opening or waiting")
	}
	return nil
}
