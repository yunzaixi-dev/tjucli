//go:build windows

package workspaceconfig

// These tests run natively on Windows CI with the standard Go test runner.
// They use only temporary files and Win32 calls, never icacls/PowerShell, a
// shell, another account's credentials or SID-bearing test diagnostics.
import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

func testUserSID(t *testing.T) string {
	t.Helper()
	identity, err := currentTokenIdentity()
	if err != nil {
		t.Fatal("test token identity unavailable")
	}
	sid, err := identity.user.String()
	if err != nil {
		t.Fatal("test SID conversion unavailable")
	}
	return sid
}

func setTestDACL(t *testing.T, path string, directory bool, sddl string, protected bool) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal("test ACL fixture open failed")
	}
	defer f.Close()
	h, err := writableSecurityHandle(f, directory, false)
	if err != nil {
		t.Fatal("test writable security handle unavailable")
	}
	defer syscall.CloseHandle(h)
	information := uintptr(securityDACL)
	if protected {
		information |= securityProtectDACL
	} else {
		information |= 0x20000000 // UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	// Empty fixture intentionally applies a null (allow-everyone) DACL.
	var dacl, descriptor unsafe.Pointer
	if sddl != "" {
		text, err := syscall.UTF16PtrFromString(sddl)
		if err != nil {
			t.Fatal("test security descriptor fixture invalid")
		}
		ok, _, _ := convertSDProc.Call(uintptr(unsafe.Pointer(text)), 1, uintptr(unsafe.Pointer(&descriptor)), 0)
		runtime.KeepAlive(text)
		if ok == 0 || descriptor == nil {
			t.Fatal("test security descriptor construction failed")
		}
		defer syscall.LocalFree(syscall.Handle(uintptr(descriptor)))
		var present, defaulted int32
		ok, _, _ = getSDDACLProc.Call(uintptr(descriptor), uintptr(unsafe.Pointer(&present)), uintptr(unsafe.Pointer(&dacl)), uintptr(unsafe.Pointer(&defaulted)))
		if ok == 0 || present == 0 {
			t.Fatal("test DACL construction failed")
		}
	}
	status, _, _ := setSecurityInfoProc.Call(uintptr(h), seFileObject, information, 0, 0, uintptr(dacl), 0)
	if status != 0 {
		t.Fatal("test DACL application failed")
	}
}

func TestWindowsConfigUsesProtectedOwnerOnlyACLs(t *testing.T) {
	dir, c := newConfig(t)
	identity, err := currentTokenIdentity()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, filepath.Join(dir, StateDirName), filepath.Join(dir, ConfigFileName), filepath.Join(dir, lockFileName)} {
		directory := path == dir || path == filepath.Join(dir, StateDirName)
		f, err := os.Open(path)
		if err != nil {
			t.Fatal("private object open failed")
		}
		s, err := readFileSecurity(syscall.Handle(f.Fd()))
		if err != nil {
			f.Close()
			t.Fatal(err)
		}
		if !sameSID(s.owner, identity.user) {
			s.close()
			f.Close()
			t.Fatal("private object has a foreign/default-group owner")
		}
		s.close()
		if err := checkWindowsACL(syscall.Handle(f.Fd()), directory, identity, false); err != nil {
			f.Close()
			t.Fatal("private object lacks a protected owner-only ACL")
		}
		f.Close()
	}
	// Save and Update must preserve actual ACLs through atomic replacements.
	c.Name = "saved"
	if err := Save(dir, c); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(dir, func(c *Config) error { c.Name = "updated"; return nil }); err != nil {
		t.Fatal(err)
	}
	if CheckPrivateDir(dir) != nil || CheckPrivateFile(filepath.Join(dir, ConfigFileName)) != nil {
		t.Fatal("atomic publication lost private ACLs")
	}
}

func TestWindowsReadChecksRejectInsecureACLsWithoutRepair(t *testing.T) {
	sid := testUserSID(t)
	for _, tc := range []struct {
		name      string
		sddl      string
		protected bool
	}{
		{"everyone-read", "D:P(A;;FA;;;" + sid + ")(A;;FR;;;WD)", true},
		{"everyone-only", "D:P(A;;FA;;;WD)", true},
		{"administrators-read", "D:P(A;;FA;;;" + sid + ")(A;;FR;;;BA)", true},
		{"system-read", "D:P(A;;FA;;;" + sid + ")(A;;FR;;;SY)", true},
		{"null-dacl", "", true},
		{"unprotected-dacl", "D:(A;;FA;;;" + sid + ")", false},
		{"owner-readonly", "D:P(A;;FR;;;" + sid + ")", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, c := newConfig(t)
			path := filepath.Join(dir, ConfigFileName)
			setTestDACL(t, path, false, tc.sddl, tc.protected)
			if err := CheckPrivateFile(path); err == nil {
				t.Fatal("Check accepted an insecure/noncanonical ACL")
			}
			if _, err := Load(dir); err == nil || strings.Contains(err.Error(), sid) {
				t.Fatal("Load accepted insecure ACL or disclosed SID")
			}
			called := false
			if _, err := Update(dir, func(*Config) error { called = true; return nil }); err == nil || called {
				t.Fatal("Update read unsafe config or invoked mutation callback")
			}
			if CheckPrivateFile(path) == nil {
				t.Fatal("read-only checks silently repaired unsafe ACL")
			}
			// Explicit local-owner repair does not read/change contents.
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal("fixture content inaccessible")
			}
			if err := EnsurePrivateFile(path); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("ACL repair changed contents")
			}
			if err := Save(dir, c); err != nil {
				t.Fatal(err)
			}
			if err := CheckPrivateFile(path); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWindowsUnsafeDirectoryAndLockACLsFailClosed(t *testing.T) {
	sid := testUserSID(t)
	t.Run("directory", func(t *testing.T) {
		dir, _ := newConfig(t)
		setTestDACL(t, dir, true, "D:P(A;OICI;FA;;;"+sid+")(A;OICI;FR;;;WD)", true)
		if CheckPrivateDir(dir) == nil {
			t.Fatal("Check accepted shared directory")
		}
		if _, err := Load(dir); err == nil {
			t.Fatal("Load accepted shared config directory")
		}
		called := false
		if _, err := Update(dir, func(*Config) error { called = true; return nil }); err == nil || called {
			t.Fatal("Update accepted shared directory")
		}
		if CheckPrivateDir(dir) == nil {
			t.Fatal("checks repaired shared directory")
		}
		if err := EnsurePrivateDir(dir); err != nil {
			t.Fatal(err)
		}
		if err := CheckPrivateDir(dir); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("lock", func(t *testing.T) {
		dir, c := newConfig(t)
		path := filepath.Join(dir, lockFileName)
		setTestDACL(t, path, false, "D:P(A;;FA;;;"+sid+")(A;;FR;;;WD)", true)
		before, _ := os.ReadFile(filepath.Join(dir, ConfigFileName))
		if Save(dir, c) == nil {
			t.Fatal("Save accepted unsafe lock ACL")
		}
		if _, err := Init(dir, c.Root, "replacement"); err == nil {
			t.Fatal("Init accepted unsafe lock ACL")
		}
		called := false
		if _, err := Update(dir, func(*Config) error { called = true; return nil }); err == nil || called {
			t.Fatal("Update accepted unsafe lock ACL")
		}
		after, _ := os.ReadFile(filepath.Join(dir, ConfigFileName))
		if !bytes.Equal(before, after) || CheckPrivateFile(path) == nil {
			t.Fatal("unsafe lock was repaired or config was published")
		}
	})
}

func TestWindowsInheritedLockFinalizationAndPrivateChildCreation(t *testing.T) {
	dir, _ := newConfig(t)
	path := filepath.Join(dir, lockFileName)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := currentTokenIdentity()
	if err != nil {
		f.Close()
		t.Fatal(err)
	}
	// The private directory must prevent disclosure even before protection
	// of a newly created child is finalized.
	if err := checkWindowsACL(syscall.Handle(f.Fd()), false, identity, true); err != nil {
		f.Close()
		t.Fatal("fresh child inherited non-owner access")
	}
	f.Close()
	if _, err := Update(dir, func(c *Config) error { c.Name = "lock-finalized"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivateFile(path); err != nil {
		t.Fatal("inherited private lock ACL was not finalized")
	}
}

func TestWindowsPrivateHelpersRejectAlternateStreams(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "file")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateFile(path); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []func(string) error{EnsurePrivateFile, CheckPrivateFile} {
		if operation(path+":stream") == nil {
			t.Fatal("helper accepted alternate-stream alias")
		}
	}
	if _, err := os.Stat(path + ":stream"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("helper created alternate stream")
	}
	if err := CheckPrivateFile(path); err != nil {
		t.Fatal("alternate-stream rejection changed base ACL")
	}
}

func TestWindowsPermissionChecksRejectWrongObjectAndIdentity(t *testing.T) {
	dir, _ := newConfig(t)
	f, err := os.Open(filepath.Join(dir, ConfigFileName))
	if err != nil {
		t.Fatal("private fixture open failed")
	}
	defer f.Close()
	if ensurePrivatePermissions(f, true) == nil || checkPrivatePermissions(f, true) == nil {
		t.Fatal("directory policy was applied to a regular file")
	}
	identity, err := currentTokenIdentity()
	if err != nil {
		t.Fatal(err)
	}
	other, err := syscall.StringToSid("S-1-1-0") // well-known Everyone, not a user SID
	if err != nil {
		t.Fatal("well-known identity fixture unavailable")
	}
	if err := checkWindowsACL(syscall.Handle(f.Fd()), false, tokenIdentity{other, other}, false); err == nil {
		t.Fatal("ACL validator accepted another principal's ownership")
	}
	if err := checkWindowsACL(syscall.Handle(f.Fd()), false, identity, false); err != nil {
		t.Fatal("rejected checks changed the private file's ACL")
	}
}
