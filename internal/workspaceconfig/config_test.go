package workspaceconfig

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func newConfig(t *testing.T) (string, Config) {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "environment")
	c, err := Init(dir, base, "测试环境")
	if err != nil {
		t.Fatal(err)
	}
	return dir, c
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return // Windows ACLs are not represented by POSIX permission bits.
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("%s: permissions %o, want %o", path, info.Mode().Perm(), want)
	}
}

func TestInitIndependentIdentityAndState(t *testing.T) {
	base := t.TempDir()
	firstDir := filepath.Join(base, "one")
	first, err := Init(firstDir, base, "one")
	if err != nil {
		t.Fatal(err)
	}
	secondDir := filepath.Join(base, "two")
	second, err := Init(secondDir, base, "two")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID || len(first.ID) != 32 || first.Root != second.Root {
		t.Fatal("identity must belong to the config directory, not the project")
	}
	if len(first.AllowedCapabilities) != 0 || len(first.Plugins) != 0 || len(first.Endpoints) != 0 || len(first.MCPServers) != 0 {
		t.Fatal("new environments must be empty and deny remote execution")
	}
	for _, dir := range []string{firstDir, secondDir} {
		assertMode(t, dir, 0700)
		assertMode(t, filepath.Join(dir, StateDirName), 0700)
		assertMode(t, filepath.Join(dir, ConfigFileName), 0600)
	}
	first.Plugins = []string{"@example/already-installed", filepath.Join(base, "local-plugin")}
	first.Endpoints = []Endpoint{{Name: "local", BaseURL: "http://127.0.0.1:11434/v1", Model: "test"}}
	if err := Save(firstDir, first); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(firstDir)
	if err != nil || !reflect.DeepEqual(loaded, first) {
		t.Fatalf("round trip: %v, %#v", err, loaded)
	}
	other, err := Load(secondDir)
	if err != nil || len(other.Plugins) != 0 || len(other.Endpoints) != 0 {
		t.Fatal("configuration leaked between environments")
	}
	if _, err := Init(firstDir, base, "replacement"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("Init must not overwrite an identity: %v", err)
	}
	still, _ := Load(firstDir)
	if still.ID != first.ID || still.Name != first.Name {
		t.Fatal("Init replaced existing configuration")
	}
}

func TestPublicRedactsAndDetaches(t *testing.T) {
	_, c := newConfig(t)
	c.APIBaseURL = "https://example.com/api"
	c.ConnectionToken = "private-connection-token"
	c.MCPServers["test"] = MCPServer{
		Command: "installed-server",
		Args:    []string{"--stdio", "--token", "private-argv-secret"},
		Env:     map[string]string{"API_KEY": "PRIVATE_SECRET_REFERENCE"},
	}
	c.Endpoints = []Endpoint{{Name: "test", BaseURL: "https://example.com/v1", Model: "test", APIKeyEnv: "MODEL_KEY"}}
	before, _ := json.Marshal(c)
	public := Public(c)
	data, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{c.ConnectionToken, "connection_token", "PRIVATE_SECRET_REFERENCE", "private-argv-secret", "--token", `"args"`} {
		if strings.Contains(string(data), secret) {
			t.Fatal("Public exposed a private token, environment value or argv")
		}
	}
	if !strings.Contains(string(data), "API_KEY") || !strings.Contains(string(data), "MODEL_KEY") {
		t.Fatal("Public should retain safe variable names")
	}
	// Public contains independently owned slices and maps.
	v := reflect.ValueOf(public)
	v.FieldByName("Endpoints").Index(0).FieldByName("Model").SetString("modified")
	v.FieldByName("MCPServers").SetMapIndex(reflect.ValueOf("test"), reflect.Value{})
	after, _ := json.Marshal(c)
	if string(before) != string(after) {
		t.Fatal("Public mutated the private configuration")
	}
}

func TestValidateRejectsUnsafeConfiguration(t *testing.T) {
	_, initial := newConfig(t)
	cases := []struct {
		name   string
		change func(*Config)
	}{
		{"version", func(c *Config) { c.Version++ }},
		{"identity", func(c *Config) { c.ID = "../identity" }},
		{"name", func(c *Config) { c.Name = "\n" }},
		{"root-relative", func(c *Config) { c.Root = "." }},
		{"token-no-api", func(c *Config) { c.ConnectionToken = "token" }},
		{"token-header-injection", func(c *Config) { c.ConnectionToken = "token\r\nHeader:x" }},
		{"unsupported-capability", func(c *Config) { c.AllowedCapabilities = []string{"exec"} }},
		{"duplicate-capability", func(c *Config) { c.AllowedCapabilities = []string{"mcp.call", "mcp.call"} }},
		{"plaintext-key", func(c *Config) { c.Endpoints[0].APIKeyEnv = "sk-literal-key" }},
		{"env-value", func(c *Config) {
			c.MCPServers["test"] = MCPServer{Command: "server", Env: map[string]string{"KEY": "sk-literal-key"}}
		}},
		{"env-interpolation", func(c *Config) {
			c.MCPServers["test"] = MCPServer{Command: "server", Env: map[string]string{"KEY": "${API_KEY}"}}
		}},
		{"env-invalid-name", func(c *Config) {
			c.MCPServers["test"] = MCPServer{Command: "server", Env: map[string]string{"KEY=VALUE": "KEY"}}
		}},
		{"shell-expression", func(c *Config) { c.MCPServers["test"] = MCPServer{Command: "server | other"} }},
		{"relative-executable", func(c *Config) { c.MCPServers["test"] = MCPServer{Command: "./server"} }},
		{"arg-control", func(c *Config) { c.MCPServers["test"] = MCPServer{Command: "server", Args: []string{"bad\x00arg"}} }},
		{"arg-limit", func(c *Config) {
			c.MCPServers["test"] = MCPServer{Command: "server", Args: []string{strings.Repeat("x", 8193)}}
		}},
		{"plugin-control", func(c *Config) { c.Plugins = []string{"package\nrun"} }},
		{"plugin-duplicate", func(c *Config) { c.Plugins = []string{"package", "package"} }},
		{"endpoint-duplicate", func(c *Config) { c.Endpoints = append(c.Endpoints, c.Endpoints[0]) }},
		{"endpoint-name", func(c *Config) { c.Endpoints[0].Name = "../name" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := initial
			c.Endpoints = []Endpoint{{Name: "test", BaseURL: "https://example.com/v1", Model: "test"}}
			c.MCPServers = map[string]MCPServer{}
			tc.change(&c)
			if Validate(c) == nil {
				t.Fatal("accepted unsafe config")
			}
		})
	}
}

func TestURLValidation(t *testing.T) {
	_, initial := newConfig(t)
	badURLs := []string{
		"", "/api", "//example.com", "file:///etc/passwd", "ftp://example.com",
		"http://example.com", "https://user:secret@example.com/v1",
		"https://example.com?key=secret", "https://example.com?", "https://example.com#secret",
		"https://example.com#", "https://example.com\\evil", " https://example.com",
		"https://example.com/\r\n", "https://example.com:0", "https://example.com:65536",
		"https://example.com:abc", "https://example.com:", "https://[::]", "http://0.0.0.0",
		"https://169.254.169.254/latest", "https://[fe80::1]", "https://224.0.0.1",
		"https://2130706433", "https://127.1", "https://0x7f000001",
		"https://-host.example", "https://a..example", "https://example.com/a/../b",
		"https://example.com/%2e%2e/secrets", "https://example.com/%00",
	}
	for _, raw := range badURLs {
		t.Run(raw, func(t *testing.T) {
			c := initial
			c.Endpoints = []Endpoint{{Name: "test", BaseURL: raw, Model: "test"}}
			if Validate(c) == nil {
				t.Fatal("accepted unsafe endpoint URL")
			}
		})
	}
	for _, raw := range []string{
		"https://example.com/v1", "http://localhost:11434/v1", "http://127.0.0.1:11434/v1",
		"http://[::1]:8080/v1", "http://192.168.1.2:8080/v1", "https://[::ffff:127.0.0.1]:8080",
	} {
		c := initial
		c.Endpoints = []Endpoint{{Name: "local-test", BaseURL: raw, Model: "test", APIKeyEnv: "MY_KEY"}}
		if err := Validate(c); err != nil {
			t.Fatalf("local execution endpoint %q rejected: %v", raw, err)
		}
	}
	c := initial
	c.APIBaseURL = "http://192.168.1.2/api"
	if Validate(c) == nil {
		t.Fatal("API connection must not send bearer tokens over non-loopback HTTP")
	}
	c.APIBaseURL = "http://localhost:8080/api"
	if err := Validate(c); err != nil {
		t.Fatal(err)
	}
}

func TestLoadBoundsStrictJSONAndPermissions(t *testing.T) {
	dir, c := newConfig(t)
	path := filepath.Join(dir, ConfigFileName)
	for _, data := range []string{
		`{"secret-unknown-field":"do-not-leak-this-value"}`,
		`{"version":"do-not-leak-this-value"}`,
		`{`,
		`null`,
		`[]`,
		strings.Repeat(" ", MaxConfigBytes+1),
	} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(dir)
		if err == nil || strings.Contains(err.Error(), "do-not-leak-this-value") {
			t.Fatalf("invalid or secret-bearing JSON not safely rejected: %v", err)
		}
	}
	data, _ := json.Marshal(c)
	if err := os.WriteFile(path, append(data, []byte("\n{}")...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("accepted multiple JSON documents")
	}
	if err := Save(dir, c); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir); err == nil {
			t.Fatal("accepted world-readable config")
		}
		if err := Save(dir, c); err != nil {
			t.Fatal(err)
		}
		assertMode(t, path, 0600)
		if err := os.Chmod(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir); err == nil {
			t.Fatal("accepted world-searchable config directory")
		}
	}
}

func TestConfigSymlinkDefenses(t *testing.T) {
	for _, kind := range []string{"config", "state", "directory", "ancestor", "broken-config"} {
		t.Run(kind, func(t *testing.T) {
			dir, c := newConfig(t)
			outside := t.TempDir()
			victim := filepath.Join(outside, "victim")
			if err := os.WriteFile(victim, []byte("unchanged"), 0600); err != nil {
				t.Fatal(err)
			}
			checkDir := dir
			link := func(target, name string) {
				t.Helper()
				if err := os.Symlink(target, name); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
			}
			switch kind {
			case "config", "broken-config":
				if err := os.Remove(filepath.Join(dir, ConfigFileName)); err != nil {
					t.Fatal(err)
				}
				target := victim
				if kind == "broken-config" {
					target = filepath.Join(outside, "missing")
				}
				link(target, filepath.Join(dir, ConfigFileName))
			case "state":
				if err := os.Remove(filepath.Join(dir, StateDirName)); err != nil {
					t.Fatal(err)
				}
				link(outside, filepath.Join(dir, StateDirName))
			case "directory":
				checkDir = filepath.Join(filepath.Dir(dir), "linked")
				link(dir, checkDir)
			case "ancestor":
				parent := filepath.Join(filepath.Dir(dir), "linked-parent")
				link(filepath.Dir(dir), parent)
				checkDir = filepath.Join(parent, filepath.Base(dir))
			}
			if err := Save(checkDir, c); err == nil {
				t.Fatal("Save accepted symlink")
			}
			if _, err := Init(checkDir, c.Root, "replacement"); err == nil {
				t.Fatal("Init accepted symlink")
			}
			// State isn't accessed by Load; runtime opens it separately.
			if kind != "state" {
				if _, err := Load(checkDir); err == nil {
					t.Fatal("Load accepted symlink")
				}
			}
			data, _ := os.ReadFile(victim)
			if string(data) != "unchanged" {
				t.Fatal("modified symlink target")
			}
		})
	}
}

func TestInvalidSaveDoesNotTouchConfiguration(t *testing.T) {
	dir, c := newConfig(t)
	before, _ := os.ReadFile(filepath.Join(dir, ConfigFileName))
	c.Version++
	if Save(dir, c) == nil {
		t.Fatal("saved invalid configuration")
	}
	after, _ := os.ReadFile(filepath.Join(dir, ConfigFileName))
	if string(before) != string(after) {
		t.Fatal("invalid Save touched existing configuration")
	}
	for _, dir := range []string{"", ".", "/", t.TempDir() + "/x/../y"} {
		if _, err := OpenPrivateDir(dir); err == nil {
			t.Fatalf("accepted unsafe configuration path %q", dir)
		}
	}
}

func TestConcurrentInitPublishesOnce(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "nested", "environment")
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := Init(dir, base, "environment")
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, os.ErrExist) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("concurrent Init succeeded %d times", success)
	}
	if _, err := Load(dir); err != nil {
		t.Fatal(err)
	}
	assertMode(t, filepath.Dir(dir), 0700)
	files, _ := os.ReadDir(dir)
	if len(files) != 3 {
		t.Fatal("temporary config files left behind")
	}
}

func TestAtomicSaveWithConcurrentReaders(t *testing.T) {
	dir, c := newConfig(t)
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	for worker := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := c
			local.Name = strings.Repeat(string(rune('a'+worker)), 200)
			for range 20 {
				if err := Save(dir, local); err != nil {
					failures <- err
					return
				}
				if _, err := Load(dir); err != nil {
					failures <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 3 {
		t.Fatal("temporary config files left behind")
	}
}
