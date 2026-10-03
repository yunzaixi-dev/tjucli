package workspaceconfig

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Child test processes only use temporary configurations and file markers.
// They do not invoke a shell, read stdin, start models or contact any API.
func TestConfigWriterProcess(t *testing.T) {
	mode := os.Getenv("TJUCLAW_CONFIG_TEST_MODE")
	if mode == "" {
		return
	}
	dir := os.Getenv("TJUCLAW_CONFIG_TEST_DIR")
	marker := os.Getenv("TJUCLAW_CONFIG_TEST_MARKER")
	if err := os.WriteFile(marker+".entered", nil, 0600); err != nil {
		os.Exit(10)
	}
	var err error
	switch mode {
	case "increment":
		for range 12 {
			_, err = Update(dir, func(c *Config) error {
				n, err := strconv.Atoi(c.Name)
				if err != nil {
					return err
				}
				c.Name = strconv.Itoa(n + 1)
				return nil
			})
			if err != nil {
				break
			}
		}
	case "hold-revoke":
		_, err = Update(dir, func(c *Config) error {
			c.AllowedCapabilities = []string{}
			c.APIBaseURL, c.ConnectionToken = "", ""
			c.RuntimeAuth = nil
			if err := os.WriteFile(marker+".held", nil, 0600); err != nil {
				return err
			}
			// Test-only instrumentation widens the race deterministically.
			// Production callbacks must not wait on external I/O.
			return waitForMarker(marker + ".release")
		})
	case "configure":
		_, err = Update(dir, func(c *Config) error {
			c.Plugins = []string{"already-installed-plugin"}
			c.Endpoints = []Endpoint{{Name: "local", BaseURL: "http://localhost:11434/v1", Model: "test"}}
			return nil
		})
	case "save-revoke":
		var c Config
		c, err = Load(dir)
		if err == nil {
			c.Name = "saved"
			c.AllowedCapabilities = []string{}
			c.APIBaseURL, c.ConnectionToken = "", ""
			c.RuntimeAuth = nil
			err = Save(dir, c)
		}
	case "init":
		_, err = Init(dir, os.Getenv("TJUCLAW_CONFIG_TEST_ROOT"), "replacement")
		if errors.Is(err, os.ErrExist) {
			err = nil // the existing identity must survive the attempt
		} else if err == nil {
			err = errors.New("unexpected identity replacement")
		}
	default:
		err = errors.New("unknown test mode")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "config helper failed:", err)
		os.Exit(11)
	}
	os.Exit(0)
}

func waitForMarker(path string) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		time.Sleep(5 * time.Millisecond)
	}
	return errors.New("test marker timeout")
}

type writerProcess struct {
	cmd    *exec.Cmd
	output bytes.Buffer
	done   chan error
	marker string
}

func startWriter(t *testing.T, dir, root, mode string) *writerProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := &writerProcess{
		cmd:    exec.Command(executable, "-test.run=^TestConfigWriterProcess$"),
		done:   make(chan error, 1),
		marker: filepath.Join(t.TempDir(), "marker"),
	}
	p.cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"SystemRoot=" + os.Getenv("SystemRoot"),
		"GORACE=atexit_sleep_ms=0",
		"TJUCLAW_CONFIG_TEST_MODE=" + mode,
		"TJUCLAW_CONFIG_TEST_DIR=" + dir,
		"TJUCLAW_CONFIG_TEST_ROOT=" + root,
		"TJUCLAW_CONFIG_TEST_MARKER=" + p.marker,
	}
	p.cmd.Stdout, p.cmd.Stderr = &p.output, &p.output
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.done <- p.cmd.Wait() }()
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })
	return p
}

func (p *writerProcess) wait(t *testing.T) {
	t.Helper()
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatalf("writer failed: %v, %s", err, p.output.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("writer blocked or deadlocked")
	}
}

func (p *writerProcess) blocked(t *testing.T) {
	t.Helper()
	if err := waitForMarker(p.marker + ".entered"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-p.done:
		t.Fatalf("writer did not respect held cross-process lock: %v, %s", err, p.output.String())
	case <-time.After(100 * time.Millisecond):
	}
}

func TestUpdatePreservesCurrentFieldsAndPrivatePublication(t *testing.T) {
	dir, c := newConfig(t)
	c.AllowedCapabilities = []string{"mcp.call"}
	c.APIBaseURL, c.ConnectionToken = "https://example.invalid/api", "test-only-token"
	if err := Save(dir, c); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(dir, func(c *Config) error {
		c.AllowedCapabilities = []string{}
		c.APIBaseURL, c.ConnectionToken = "", ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	updated, err := Update(dir, func(current *Config) error {
		// Reading a current configuration is allowed. Writer recursion isn't.
		if loaded, err := Load(dir); err != nil || !reflect.DeepEqual(loaded, *current) {
			return errors.New("callback did not receive current configuration")
		}
		current.Plugins = []string{"installed-plugin"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.AllowedCapabilities) != 0 || updated.ConnectionToken != "" || updated.ID != c.ID {
		t.Fatal("unrelated update restored revoked credentials or permissions")
	}
	loaded, err := Load(dir)
	if err != nil || !reflect.DeepEqual(loaded, updated) {
		t.Fatal("Update result differs from published configuration")
	}
	assertMode(t, filepath.Join(dir, lockFileName), 0600)
	assertMode(t, filepath.Join(dir, ConfigFileName), 0600)
	assertMode(t, dir, 0700)
}

func TestUpdateErrorsLeaveOriginalAndReleaseLock(t *testing.T) {
	dir, _ := newConfig(t)
	original, _ := os.ReadFile(filepath.Join(dir, ConfigFileName))
	callbackErr := errors.New("test callback failure")
	for _, tc := range []struct {
		name   string
		mutate func(*Config) error
		want   error
	}{
		{"callback-error", func(c *Config) error { c.Name = "not-published"; return callbackErr }, callbackErr},
		{"invalid-config", func(c *Config) error { c.Version++; return nil }, nil},
		{"nil-callback", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Update(dir, tc.mutate)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) || !reflect.DeepEqual(result, Config{}) {
				t.Fatal("Update did not reject callback or invalid config")
			}
			data, _ := os.ReadFile(filepath.Join(dir, ConfigFileName))
			if !bytes.Equal(data, original) {
				t.Fatal("failed Update changed configuration")
			}
			if _, err := Update(dir, func(*Config) error { return nil }); err != nil {
				t.Fatalf("failed Update retained lock: %v", err)
			}
		})
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("callback panic was unexpectedly swallowed")
			}
		}()
		_, _ = Update(dir, func(c *Config) error {
			c.Name = "not-published"
			panic("test panic")
		})
	}()
	if _, err := Update(dir, func(*Config) error { return nil }); err != nil {
		t.Fatalf("panic retained lock: %v", err)
	}
}

func TestUpdateDoesNotCreateMissingConfiguration(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	called := false
	_, err := Update(dir, func(*Config) error { called = true; return nil })
	if err == nil || called {
		t.Fatal("Update initialized missing configuration")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Update created missing configuration directory")
	}
}

func TestConcurrentProcessesUpdateWithoutLostChanges(t *testing.T) {
	dir, _ := newConfig(t)
	if _, err := Update(dir, func(c *Config) error { c.Name = "0"; return nil }); err != nil {
		t.Fatal(err)
	}
	var writers []*writerProcess
	for range 6 {
		writers = append(writers, startWriter(t, dir, "", "increment"))
	}
	for _, p := range writers {
		p.wait(t)
	}
	result, err := Load(dir)
	if err != nil || result.Name != "72" {
		t.Fatalf("lost cross-process updates: count=%q, err=%v", result.Name, err)
	}
}

func TestPendingProcessUpdateCannotRestoreRevocation(t *testing.T) {
	dir, c := newConfig(t)
	c.AllowedCapabilities = []string{"mcp.call"}
	c.APIBaseURL, c.ConnectionToken = "https://example.invalid/api", "test-only-token"
	c.RuntimeAuth = map[string]RuntimeAuth{"claude": {Mode: "oauth-env", Source: "OWNER_AUTH_REF"}}
	if err := Save(dir, c); err != nil {
		t.Fatal(err)
	}
	holder := startWriter(t, dir, c.Root, "hold-revoke")
	if err := waitForMarker(holder.marker + ".held"); err != nil {
		t.Fatal(err)
	}
	pending := startWriter(t, dir, c.Root, "configure")
	pending.blocked(t)
	if err := os.WriteFile(holder.marker+".release", nil, 0600); err != nil {
		t.Fatal(err)
	}
	holder.wait(t)
	pending.wait(t)
	current, err := Load(dir)
	if err != nil || len(current.AllowedCapabilities) != 0 || current.ConnectionToken != "" || current.APIBaseURL != "" || len(current.RuntimeAuth) != 0 {
		t.Fatal("pending configure resurrected permissions or token")
	}
	if len(current.Plugins) != 1 || len(current.Endpoints) != 1 {
		t.Fatal("pending configure was not published")
	}
}

func TestSaveAndInitProcessesRespectUpdateLock(t *testing.T) {
	for _, mode := range []string{"save-revoke", "init"} {
		t.Run(mode, func(t *testing.T) {
			dir, c := newConfig(t)
			c.AllowedCapabilities = []string{"mcp.call"}
			c.APIBaseURL, c.ConnectionToken = "https://example.invalid/api", "test-only-token"
			c.RuntimeAuth = map[string]RuntimeAuth{"claude": {Mode: "oauth-env", Source: "OWNER_AUTH_REF"}}
			if err := Save(dir, c); err != nil {
				t.Fatal(err)
			}
			holder := startWriter(t, dir, c.Root, "hold-revoke")
			if err := waitForMarker(holder.marker + ".held"); err != nil {
				t.Fatal(err)
			}
			pending := startWriter(t, dir, c.Root, mode)
			pending.blocked(t)
			if err := os.WriteFile(holder.marker+".release", nil, 0600); err != nil {
				t.Fatal(err)
			}
			holder.wait(t)
			pending.wait(t)
			// A subsequent field update must preserve the serialized Save's
			// revocation, and an attempted Init must not replace the identity.
			configure := startWriter(t, dir, c.Root, "configure")
			configure.wait(t)
			current, err := Load(dir)
			if err != nil || current.ID != c.ID || len(current.AllowedCapabilities) != 0 ||
				current.ConnectionToken != "" || current.APIBaseURL != "" || len(current.RuntimeAuth) != 0 {
				t.Fatal("cooperative writer restored revoked state")
			}
			if mode == "save-revoke" && current.Name != "saved" {
				t.Fatal("Save did not publish after obtaining the lock")
			}
		})
	}
}

func TestWriterExitReleasesPersistentLock(t *testing.T) {
	dir, c := newConfig(t)
	lockBefore, err := os.Stat(filepath.Join(dir, lockFileName))
	if err != nil {
		t.Fatal(err)
	}
	holder := startWriter(t, dir, c.Root, "hold-revoke")
	if err := waitForMarker(holder.marker + ".held"); err != nil {
		t.Fatal(err)
	}
	pending := startWriter(t, dir, c.Root, "configure")
	pending.blocked(t)
	if err := holder.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-holder.done:
	case <-time.After(10 * time.Second):
		t.Fatal("killed writer was not reaped")
	}
	pending.wait(t)
	lockAfter, err := os.Stat(filepath.Join(dir, lockFileName))
	if err != nil || !os.SameFile(lockBefore, lockAfter) {
		t.Fatal("writer termination removed or replaced the persistent lock")
	}
}

func TestLockFileDefenses(t *testing.T) {
	for _, kind := range []string{"symlink", "internal-symlink", "broken-symlink", "directory", "nonempty", "insecure-permissions"} {
		t.Run(kind, func(t *testing.T) {
			dir, c := newConfig(t)
			lock := filepath.Join(dir, lockFileName)
			if err := os.Remove(lock); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			victim := filepath.Join(outside, "victim")
			if err := os.WriteFile(victim, nil, 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink", "internal-symlink", "broken-symlink":
				target := victim
				if kind == "broken-symlink" {
					target = filepath.Join(outside, "missing")
				} else if kind == "internal-symlink" {
					target = ConfigFileName
				}
				if err := os.Symlink(target, lock); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
			case "directory":
				if err := os.Mkdir(lock, 0700); err != nil {
					t.Fatal(err)
				}
			case "nonempty":
				if err := os.WriteFile(lock, []byte("not-a-lock"), 0600); err != nil {
					t.Fatal(err)
				}
			case "insecure-permissions":
				if err := os.WriteFile(lock, nil, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(lock, 0644); err != nil {
					t.Fatal(err)
				}
				if info, _ := os.Stat(lock); info.Mode().Perm() != 0644 {
					t.Skip("platform does not represent POSIX permissions")
				}
			}
			before, _ := os.ReadFile(filepath.Join(dir, ConfigFileName))
			if err := Save(dir, c); err == nil {
				t.Fatal("Save accepted an unsafe lock")
			}
			if _, err := Init(dir, c.Root, "replacement"); err == nil {
				t.Fatal("Init accepted an unsafe lock")
			}
			called := false
			if _, err := Update(dir, func(*Config) error { called = true; return nil }); err == nil || called {
				t.Fatal("Update accepted an unsafe lock or ran callback")
			}
			after, _ := os.ReadFile(filepath.Join(dir, ConfigFileName))
			if !bytes.Equal(before, after) {
				t.Fatal("unsafe lock changed configuration")
			}
			info, err := os.Stat(victim)
			if err != nil || info.Size() != 0 {
				t.Fatal("modified external lock symlink target")
			}
			assertMode(t, victim, 0600)
			if _, err := os.Stat(filepath.Join(outside, "missing")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("created broken lock symlink target")
			}
		})
	}
}

func TestReplacedLockFileIsRejected(t *testing.T) {
	dir, _ := newConfig(t)
	r, err := OpenPrivateDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	f, err := openLockFile(r)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := r.Rename(lockFileName, ".old-lock"); err != nil {
		t.Skipf("platform prevents moving an open lock: %v", err)
	}
	replacement, err := r.OpenFile(lockFileName, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	replacement.Close()
	if err := checkLockFile(r, f); err == nil {
		t.Fatal("replaced lock inode was accepted")
	}
}

func TestUnsupportedPlatformsFailClosed(t *testing.T) {
	if configLockSupported {
		t.Skip("platform supports native config locking")
	}
	base := t.TempDir()
	dir := filepath.Join(base, "unsupported")
	c := Config{Version: Version, ID: "identity", Name: "unsupported", Root: base}
	if err := Save(dir, c); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatal("unsupported platform published config")
	}
	if _, err := Init(dir, base, "unsupported"); err == nil {
		t.Fatal("unsupported platform initialized config")
	}
	if _, err := Update(dir, func(*Config) error { return nil }); err == nil {
		t.Fatal("unsupported platform updated config")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unsupported platform created state before failing closed")
	}
}
