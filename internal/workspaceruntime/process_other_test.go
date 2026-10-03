//go:build !unix

package workspaceruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yunzaixi-dev/tjucli/internal/workspaceconfig"
)

func TestNonUnixPromptExecutionFailsBeforeCredentialsStateOrSession(t *testing.T) {
	if promptProcessTreeSupported {
		t.Fatal("non-Unix prompt execution enabled without invocation supervision")
	}
	for _, runtime := range []string{"pi", "claude", "codex"} {
		t.Run(runtime, func(t *testing.T) {
			state := t.TempDir()
			e := Executor{Config: workspaceconfig.Config{
				Version: 1, ID: "independent-identity", Name: "test",
				Root:                filepath.Join(t.TempDir(), "missing-root"),
				AllowedCapabilities: []string{runtime + ".prompt"},
				Endpoints: []workspaceconfig.Endpoint{{
					Name: "local", Model: "synthetic-model",
					BaseURL: "https://example.test/v1", APIKeyEnv: "TJUCLAW_TEST_UNSET_MODEL_KEY",
				}},
			}, StateDir: state}
			// A missing endpoint key or auth source would return a different
			// error if the platform gate were moved behind credential lookup.
			t.Setenv("TJUCLAW_TEST_UNSET_MODEL_KEY", "")
			for _, hostAuth := range []bool{false, true} {
				if hostAuth {
					e.Config.RuntimeAuth = map[string]workspaceconfig.RuntimeAuth{
						runtime: {Mode: "host-file", Source: filepath.Join(state, "missing-auth.json")},
					}
				}
				for _, args := range []json.RawMessage{
					[]byte(`{"prompt":"must not run"}`),
					[]byte(`{"prompt":"must not resume","session_id":"bad-session"}`),
				} {
					result, err := e.Execute(context.Background(), runtime+".prompt", args)
					if !errors.Is(err, ErrRuntimeUnavailable) || result != nil ||
						strings.Contains(err.Error(), state) {
						t.Fatal("unsupported platform reached prompt setup", err)
					}
				}
			}
			entries, err := os.ReadDir(state)
			if err != nil || len(entries) != 0 {
				t.Fatal("denied execution created state, session or auth files")
			}
			e.Config.AllowedCapabilities = nil
			if _, err := e.Execute(context.Background(), runtime+".prompt", nil); !errors.Is(err, ErrCapabilityDenied) {
				t.Fatal("platform gate weakened local default-deny", err)
			}
		})
	}
}
