package workspaceconfig

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestReusablePrivateHelpersPreserveContentsAndRejectMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private", "outbox")
	if err := CheckPrivateDir(dir); err == nil {
		t.Fatal("Check created or accepted a missing directory")
	}
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "result.json")
	if err := EnsurePrivateFile(path); err == nil {
		t.Fatal("EnsurePrivateFile accepted a missing file")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("EnsurePrivateFile created a file")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := EnsurePrivateFile(path); err != nil {
		t.Fatal(err)
	}
	fixture := []byte(`{"result":"test-only-fixture"}`)
	if _, err := f.Write(fixture); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := EnsurePrivateFile(path); err != nil {
			t.Fatal(err)
		}
		if err := CheckPrivateFile(path); err != nil {
			t.Fatal(err)
		}
		reader, err := OpenPrivateFile(path)
		if err != nil {
			t.Fatal(err)
		}
		actual, readErr := io.ReadAll(io.LimitReader(reader, int64(len(fixture)+1)))
		if reader.Close() != nil || readErr != nil || !bytes.Equal(actual, fixture) {
			t.Fatal("privacy helper read/truncated/modified contents")
		}
	}
	assertMode(t, dir, 0700)
	assertMode(t, path, 0600)
	if err := EnsurePrivateFile(dir); err == nil {
		t.Fatal("private file helper accepted a directory")
	}
}

func TestReusablePrivateHelpersRejectSymlinks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	target := filepath.Join(outside, "untouched")
	if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	fileLink := filepath.Join(dir, "file-link")
	dirLink := filepath.Join(dir, "dir-link")
	if err := os.Symlink(target, fileLink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.Symlink(outside, dirLink); err != nil {
		t.Fatal(err)
	}
	openCheck := func(path string) error {
		f, err := OpenPrivateFile(path)
		if f != nil {
			f.Close()
		}
		return err
	}
	for _, operation := range []func(string) error{EnsurePrivateFile, CheckPrivateFile, openCheck} {
		if operation(fileLink) == nil {
			t.Fatal("private file helper followed a symlink")
		}
		if operation(filepath.Join(dirLink, "untouched")) == nil {
			t.Fatal("private file helper followed an ancestor symlink")
		}
	}
	for _, operation := range []func(string) error{EnsurePrivateDir, CheckPrivateDir} {
		if operation(dirLink) == nil {
			t.Fatal("private directory helper followed a symlink")
		}
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "unchanged" {
		t.Fatal("symlink target changed")
	}
}

func TestReusablePrivateHelpersUnixChecksDoNotRepair(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows checks ACLs, not chmod bits")
	}
	dir := filepath.Join(t.TempDir(), "private")
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "file")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if CheckPrivateFile(path) == nil {
		t.Fatal("Check accepted permissive file mode")
	}
	if f, err := OpenPrivateFile(path); err == nil {
		f.Close()
		t.Fatal("Open accepted permissive file mode")
	}
	assertMode(t, path, 0644)
	if err := EnsurePrivateFile(path); err != nil {
		t.Fatal(err)
	}
	assertMode(t, path, 0600)
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if CheckPrivateDir(dir) == nil || EnsurePrivateFile(path) == nil {
		t.Fatal("Check/file repair accepted a permissive parent")
	}
	if r, err := OpenExistingPrivateDir(dir); err == nil {
		r.Close()
		t.Fatal("Open accepted permissive directory mode")
	}
	assertMode(t, dir, 0755)
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	assertMode(t, dir, 0700)
}
