package workspaceconfig

import (
	"errors"
	"os"
	"path/filepath"
)

// EnsurePrivateDir creates or secures a clean absolute directory, rejecting
// symlinks in every path component. Unix directories use 0700; Windows uses a
// protected DACL granting only the current process-token user full access.
// Existing explicit permissions on descendants are NOT recursively repaired.
// Windows foreign-owned objects and volumes without enforceable ACLs fail
// closed; a process token's default owner (possibly an elevated group) may be
// normalized to its user while securing an object. Already-open handles are
// not revoked. Administrators/root can override OS discretionary permissions.
func EnsurePrivateDir(dir string) error {
	r, err := OpenPrivateDir(dir)
	if err != nil {
		return err
	}
	return r.Close()
}

// CheckPrivateDir checks privacy without changing permissions or creating
// anything. It shares EnsurePrivateDir's path and symlink defenses.
func CheckPrivateDir(dir string) error {
	r, err := OpenExistingPrivateDir(dir)
	if err != nil {
		return err
	}
	return r.Close()
}

// OpenExistingPrivateDir opens an existing private directory without creating
// it or repairing permissions. Checks apply to the opened directory, and
// operations through the returned Root remain anchored after ancestor renames.
// The caller must close the Root.
func OpenExistingPrivateDir(dir string) (*os.Root, error) {
	return openPrivateDir(dir, false)
}

// EnsurePrivateFile secures an existing regular file without reading,
// truncating or creating it. Its parent must already be private, and symlinks
// in the path are rejected. Unix files use 0600; Windows uses an owner-only
// protected DACL. Callers writing secrets must secure the directory first and
// use exclusive creation, then secure the empty file BEFORE writing secrets.
// This function does not make a later path-based open an atomic operation.
func EnsurePrivateFile(path string) error {
	return privateFileOperation(path, true)
}

// CheckPrivateFile checks an existing regular file and its private parent
// without reading contents, changing permissions or creating anything.
func CheckPrivateFile(path string) error {
	return privateFileOperation(path, false)
}

// OpenPrivateFile opens an existing private regular file read-only. Privacy,
// type and no-follow checks apply to the actual opened handle, avoiding a
// check-then-os.Open race. The caller must close the returned file and bound
// reads. Nothing is created, repaired or read by this function.
func OpenPrivateFile(path string) (*os.File, error) {
	if !absoluteClean(path) || !privateFilePathSafe(path) {
		return nil, errors.New("private file path must be a clean absolute path")
	}
	r, err := openPrivateDir(filepath.Dir(path), false)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return openRegular(r, filepath.Base(path))
}

func privateFileOperation(path string, ensure bool) error {
	if !ensure {
		f, err := OpenPrivateFile(path)
		if err != nil {
			return err
		}
		return f.Close()
	}
	if !absoluteClean(path) || !privateFilePathSafe(path) {
		return errors.New("private file path must be a clean absolute path")
	}
	r, err := openPrivateDir(filepath.Dir(path), false)
	if err != nil {
		return err
	}
	defer r.Close()
	f, err := openRegularUnchecked(r, filepath.Base(path))
	if err != nil {
		return err
	}
	defer f.Close()
	return ensurePrivatePermissions(f, false)
}

func privateMode(directory bool) os.FileMode {
	if directory {
		return 0700
	}
	return 0600
}
