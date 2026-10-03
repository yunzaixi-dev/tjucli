package workspaceconfig

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRuntimeAuthOptInTransactionsIsolationAndProjection(t *testing.T) {
	dir, c := newConfig(t)
	otherDir, _ := newConfig(t)
	if len(c.RuntimeAuth) != 0 {
		t.Fatal("Init implicitly attached host credentials")
	}
	// This fixture is not a real credential. Config must not open, copy, or
	// check existence of selected credential sources during publication.
	source := filepath.Join(t.TempDir(), "owner-selected-private-auth.json")
	attachments := map[string]RuntimeAuth{
		"pi":     {Mode: "oauth-env", Source: "OWNER_SELECTED_PI_TOKEN", Model: "test-model"},
		"claude": {Mode: "oauth-env", Source: "OWNER_SELECTED_CLAUDE_TOKEN"},
		"codex":  {Mode: "host-file", Source: source},
	}
	selected, err := Update(dir, func(c *Config) error {
		c.RuntimeAuth = attachments
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dir)
	if err != nil || !reflect.DeepEqual(loaded, selected) {
		t.Fatalf("auth configuration round trip: %v", err)
	}
	other, err := Load(otherDir)
	if err != nil || len(other.RuntimeAuth) != 0 {
		t.Fatal("host auth selection leaked to another environment")
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatal("config publication touched credential source")
	}
	data, err := json.Marshal(Public(selected))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{source, filepath.Base(source), "OWNER_SELECTED_PI_TOKEN", "OWNER_SELECTED_CLAUDE_TOKEN", `"source"`} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatal("Public disclosed auth source")
		}
	}
	if !bytes.Contains(data, []byte(`"configured":true`)) || !bytes.Contains(data, []byte("test-model")) {
		t.Fatal("Public omitted safe auth attachment status")
	}
	view := reflect.ValueOf(Public(selected)).FieldByName("RuntimeAuth")
	view.SetMapIndex(reflect.ValueOf("pi"), reflect.Value{})
	if len(selected.RuntimeAuth) != 3 {
		t.Fatal("Public did not detach auth map")
	}
	revoked, err := Update(dir, func(c *Config) error {
		delete(c.RuntimeAuth, "codex")
		return nil
	})
	if err != nil || len(revoked.RuntimeAuth) != 2 {
		t.Fatalf("auth revocation: %v", err)
	}
	// An unrelated subsequent field update must not resurrect attachments.
	if _, err := Update(dir, func(c *Config) error {
		c.Name = "renamed"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err = Load(dir)
	if err != nil || len(loaded.RuntimeAuth) != 2 || loaded.RuntimeAuth["codex"].Mode != "" {
		t.Fatal("subsequent Update resurrected host auth")
	}
	assertMode(t, dir, 0700)
	assertMode(t, filepath.Join(dir, ConfigFileName), 0600)
	assertMode(t, filepath.Join(dir, lockFileName), 0600)
}

func TestRuntimeAuthValidationRejectsUnsafeSchema(t *testing.T) {
	dir, c := newConfig(t)
	path := filepath.Join(dir, ConfigFileName)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	validFile := filepath.Join(t.TempDir(), "auth.json")
	for _, tc := range []struct {
		name    string
		runtime string
		auth    RuntimeAuth
	}{
		{"unknown-runtime", "other", RuntimeAuth{Mode: "host-file", Source: validFile}},
		{"no-mode", "pi", RuntimeAuth{Source: validFile}},
		{"inherit", "claude", RuntimeAuth{Mode: "inherit"}},
		{"directory-mode", "codex", RuntimeAuth{Mode: "host-home", Source: filepath.Dir(validFile)}},
		{"relative-path", "codex", RuntimeAuth{Mode: "host-file", Source: "auth.json"}},
		{"unclean-path", "codex", RuntimeAuth{Mode: "host-file", Source: filepath.Dir(validFile) + "/../auth.json"}},
		{"root-path", "codex", RuntimeAuth{Mode: "host-file", Source: filepath.VolumeName(validFile) + string(filepath.Separator)}},
		{"control-path", "codex", RuntimeAuth{Mode: "host-file", Source: validFile + "\n"}},
		{"long-path", "codex", RuntimeAuth{Mode: "host-file", Source: validFile + strings.Repeat("x", 4097)}},
		{"empty-env", "claude", RuntimeAuth{Mode: "oauth-env"}},
		{"literal-token", "claude", RuntimeAuth{Mode: "oauth-env", Source: "literal-token-value"}},
		{"interpolation", "claude", RuntimeAuth{Mode: "oauth-env", Source: "${HOST_TOKEN}"}},
		{"assignment", "pi", RuntimeAuth{Mode: "oauth-env", Source: "TOKEN=value"}},
		{"control-model", "pi", RuntimeAuth{Mode: "oauth-env", Source: "HOST_TOKEN", Model: "secret\nvalue"}},
		{"space-model", "pi", RuntimeAuth{Mode: "oauth-env", Source: "HOST_TOKEN", Model: " model "}},
		{"long-model", "pi", RuntimeAuth{Mode: "oauth-env", Source: "HOST_TOKEN", Model: strings.Repeat("x", 257)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := c
			bad.RuntimeAuth = map[string]RuntimeAuth{tc.runtime: tc.auth}
			if err := Save(dir, bad); err == nil {
				t.Fatal("saved invalid auth attachment")
			} else if strings.Contains(err.Error(), tc.auth.Source) && tc.auth.Source != "" {
				t.Fatal("validation disclosed auth source")
			}
			if _, err := Update(dir, func(c *Config) error {
				c.RuntimeAuth = bad.RuntimeAuth
				return nil
			}); err == nil {
				t.Fatal("Update accepted invalid auth attachment")
			}
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(data, original) {
				t.Fatal("invalid auth attachment changed configuration")
			}
		})
	}
}

func TestLoadAuthAttachmentRejectsUnknownFieldsWithoutLeak(t *testing.T) {
	dir, c := newConfig(t)
	data, _ := json.Marshal(c)
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	document["runtime_auth"] = map[string]any{
		"codex": map[string]any{"mode": "oauth-env", "source": "TOKEN", "token": "private-test-value"},
	}
	data, _ = json.Marshal(document)
	if err := os.WriteFile(filepath.Join(dir, ConfigFileName), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || strings.Contains(err.Error(), "private-test-value") {
		t.Fatal("Load accepted unknown literal credential field or leaked it")
	}
}

func TestPublicLinkedProjectsCredentialPresenceOnly(t *testing.T) {
	_, c := newConfig(t)
	for _, tc := range []struct {
		api, token string
		linked     bool
	}{
		{"", "", false},
		{"https://example.invalid/api", "", false},
		{"", "private-token", false},
		{"https://example.invalid/api", "private-token", true},
	} {
		c.APIBaseURL, c.ConnectionToken = tc.api, tc.token
		data, err := json.Marshal(Public(c))
		if err != nil {
			t.Fatal(err)
		}
		var public struct {
			Linked bool `json:"linked"`
		}
		if json.Unmarshal(data, &public) != nil || public.Linked != tc.linked {
			t.Fatal("Public linked flag does not reflect complete credentials")
		}
		if bytes.Contains(data, []byte("private-token")) || bytes.Contains(data, []byte("connection_token")) {
			t.Fatal("Public linked flag exposed credential")
		}
	}
}
