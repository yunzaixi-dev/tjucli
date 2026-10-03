package workspaceconfig

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// OpenPrivateDir opens or creates an absolute private directory without
// following symlinks in any component. The caller owns the returned Root.
// Operations through it remain anchored if an ancestor is subsequently renamed.
func OpenPrivateDir(dir string) (*os.Root, error) {
	return openPrivateDir(dir, true)
}

func openPrivateDir(dir string, create bool) (*os.Root, error) {
	if !absoluteClean(dir) {
		return nil, errors.New("workspace config directory must be a clean absolute path")
	}
	volume := filepath.VolumeName(dir)
	base := volume + string(filepath.Separator)
	relative := strings.TrimPrefix(dir, base)
	if relative == "" {
		return nil, errors.New("workspace config directory cannot be a filesystem root")
	}
	r, err := os.OpenRoot(base)
	if err != nil {
		return nil, fmt.Errorf("open workspace filesystem: %w", err)
	}
	parts := strings.Split(relative, string(filepath.Separator))
	for i, part := range parts {
		info, err := r.Lstat(part)
		created := false
		if errors.Is(err, os.ErrNotExist) && create {
			if err = r.Mkdir(part, 0700); err == nil {
				created = true
			} else if !errors.Is(err, os.ErrExist) {
				r.Close()
				return nil, fmt.Errorf("create workspace directory: %w", err)
			}
			info, err = r.Lstat(part)
		}
		if err != nil {
			r.Close()
			return nil, fmt.Errorf("inspect workspace directory: %w", err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			r.Close()
			return nil, errors.New("workspace directory must not contain symlinks or non-directories")
		}
		next, err := r.OpenRoot(part)
		r.Close()
		if err != nil {
			return nil, fmt.Errorf("open workspace directory: %w", err)
		}
		opened, err := next.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			next.Close()
			return nil, errors.New("workspace directory changed while opening")
		}
		if created || i == len(parts)-1 {
			f, err := next.Open(".")
			if err == nil && create {
				err = ensurePrivatePermissions(f, true)
			}
			if err == nil && !create {
				err = checkPrivatePermissions(f, true)
			}
			if f != nil {
				f.Close()
			}
			if err != nil {
				next.Close()
				return nil, fmt.Errorf("secure workspace directory: %w", err)
			}
		}
		r = next
	}
	return r, nil
}

func openRegular(r *os.Root, name string) (*os.File, error) {
	f, err := openRegularUnchecked(r, name)
	if err != nil {
		return nil, err
	}
	if err := checkPrivatePermissions(f, false); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// Type/identity checks precede any permission change; EnsurePrivateFile uses
// this without first requiring the permissions it has been asked to repair.
func openRegularUnchecked(r *os.Root, name string) (*os.File, error) {
	info, err := r.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("inspect workspace config: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("workspace config must be a regular file, not a symlink")
	}
	f, err := r.OpenFile(name, os.O_RDONLY|safeReadFlags, 0)
	if err != nil {
		return nil, fmt.Errorf("open workspace config: %w", err)
	}
	opened, err := f.Stat()
	// Atomic Save may replace a regular config between Lstat and Open. On
	// platforms with O_NOFOLLOW, the opened inode is authoritative and safe.
	if err != nil || !opened.Mode().IsRegular() || (!readNoFollow && !os.SameFile(info, opened)) {
		f.Close()
		return nil, errors.New("workspace config changed while opening")
	}
	return f, nil
}

func writeConfig(dir string, c Config, exclusive bool) error {
	if err := Validate(c); err != nil {
		return err
	}
	return withConfigLock(dir, true, func(r *os.Root) error {
		return publishConfig(r, c, exclusive)
	})
}

// publishConfig must only be called with this directory's writer lock held.
// It deliberately does not acquire another lock (Update already owns it).
func publishConfig(r *os.Root, c Config, exclusive bool) error {
	if err := Validate(c); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil || len(data)+1 > MaxConfigBytes {
		return errors.New("workspace config encoding failed or exceeds limit")
	}
	if info, err := r.Lstat(ConfigFileName); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("workspace config must be a regular file, not a symlink")
		}
		if exclusive {
			return fmt.Errorf("workspace already initialized: %w", os.ErrExist)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect workspace config: %w", err)
	}
	// State belongs to the environment, not to the project Root.
	if err := ensureChildDir(r, StateDirName); err != nil {
		return err
	}
	suffix := make([]byte, 16)
	if _, err := rand.Read(suffix); err != nil {
		return errors.New("workspace temporary filename generation failed")
	}
	temp := ".config-" + hex.EncodeToString(suffix) + ".tmp"
	f, err := r.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("create workspace config: %w", err)
	}
	defer r.Remove(temp)
	if err = ensurePrivatePermissions(f, false); err == nil {
		_, err = f.Write(append(data, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write workspace config: %w", err)
	}
	if exclusive {
		// Link gives atomic no-replace publication. The temporary file has
		// already been fully written and synced, so readers see no partial JSON.
		err = r.Link(temp, ConfigFileName)
	} else {
		err = r.Rename(temp, ConfigFileName)
	}
	if err != nil {
		return fmt.Errorf("publish workspace config: %w", err)
	}
	if exclusive {
		if err := r.Remove(temp); err != nil {
			return fmt.Errorf("clean workspace temporary config: %w", err)
		}
	}
	if runtime.GOOS != "windows" {
		d, err := r.Open(".")
		if err != nil {
			return fmt.Errorf("open workspace directory for sync: %w", err)
		}
		defer d.Close()
		if err := d.Sync(); err != nil {
			return fmt.Errorf("sync workspace directory: %w", err)
		}
	}
	return nil
}

func ensureChildDir(r *os.Root, name string) error {
	if err := r.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create workspace state: %w", err)
	}
	info, err := r.Lstat(name)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("workspace state must be a directory, not a symlink")
	}
	child, err := r.OpenRoot(name)
	if err != nil {
		return fmt.Errorf("open workspace state: %w", err)
	}
	defer child.Close()
	opened, err := child.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return errors.New("workspace state changed while opening")
	}
	f, err := child.Open(".")
	if err != nil {
		return fmt.Errorf("open workspace state permissions: %w", err)
	}
	defer f.Close()
	return ensurePrivatePermissions(f, true)
}
