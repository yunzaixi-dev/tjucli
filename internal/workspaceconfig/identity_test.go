package workspaceconfig

import (
	"strings"
	"testing"
)

func TestWorkspaceIdentityLeadingURLSafeCharactersRoundTrip(t *testing.T) {
	for _, prefix := range []string{"-", "_"} {
		t.Run(prefix, func(t *testing.T) {
			dir, original := newConfig(t)
			id := prefix + strings.Repeat("a", 31)
			updated, err := Update(dir, func(c *Config) error {
				c.ID = id
				return nil
			})
			if err != nil || updated.ID != id {
				t.Fatal("transaction rejected a URL-safe workspace identity")
			}
			loaded, err := Load(dir)
			if err != nil || loaded.ID != id || loaded.Root != original.Root {
				t.Fatal("workspace identity did not survive publication and reload")
			}
			if err := Save(dir, loaded); err != nil {
				t.Fatal("Save rejected a URL-safe workspace identity")
			}
		})
	}
}

func TestWorkspaceIdentityRejectsUnsafeAndUnboundedValues(t *testing.T) {
	dir, original := newConfig(t)
	for _, tc := range []struct{ name, id string }{
		{"empty", ""},
		{"leading-dot", ".workspace"},
		{"parent", ".."},
		{"traversal", "../workspace"},
		{"dash-traversal", "-../workspace"},
		{"underscore-traversal", "_..\\workspace"},
		{"slash", "a/b"},
		{"backslash", "a\\b"},
		{"space", "a b"},
		{"newline", "a\nb"},
		{"carriage-return", "a\rb"},
		{"nul", "a\x00b"},
		{"unicode-control", "a\u0085b"},
		{"over-bound", "_" + strings.Repeat("a", 128)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := original
			candidate.ID = tc.id
			if Validate(candidate) == nil || Save(dir, candidate) == nil {
				t.Fatal("accepted an unsafe or oversized workspace identity")
			}
			loaded, err := Load(dir)
			if err != nil || loaded.ID != original.ID {
				t.Fatal("invalid identity changed the published configuration")
			}
		})
	}
	for _, prefix := range []string{"-", "_"} {
		candidate := original
		candidate.ID = prefix + strings.Repeat("a", 127)
		if err := Validate(candidate); err != nil {
			t.Fatal("rejected an identity at the existing 128-byte bound")
		}
	}
}

func TestWorkspaceIdentityPatternDoesNotLoosenEndpointOrMCPNames(t *testing.T) {
	_, original := newConfig(t)
	for _, prefix := range []string{"-", "_"} {
		t.Run(prefix+"endpoint", func(t *testing.T) {
			candidate := original
			candidate.Endpoints = []Endpoint{{Name: prefix + "local", BaseURL: "http://127.0.0.1:11434/v1", Model: "test"}}
			if Validate(candidate) == nil {
				t.Fatal("identity change loosened endpoint names")
			}
		})
		t.Run(prefix+"server", func(t *testing.T) {
			candidate := original
			candidate.MCPServers = map[string]MCPServer{prefix + "local": {Command: "installed-server"}}
			if Validate(candidate) == nil {
				t.Fatal("identity change loosened MCP server names")
			}
		})
		t.Run(prefix+"command", func(t *testing.T) {
			if (MCPServer{Command: prefix + "executable"}).Validate() == nil {
				t.Fatal("identity change loosened MCP executable names")
			}
		})
	}
}
