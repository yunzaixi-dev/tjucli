package workspaceruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yunzaixi-dev/tjucli/internal/workspaceconfig"
)

func TestExplicitHostAuthHasNoAmbientFallbackOrSourceWriteback(t *testing.T) {
	for _, runtime := range []string{"claude", "codex"} {
		t.Run(runtime, func(t *testing.T) {
			e := fakeExecutor(t, runtime+".prompt")
			document := `{"claudeAiOauth":{"accessToken":"host-native-secret","refreshToken":"native-refresh-secret"}}`
			if runtime == "codex" {
				document = `{"auth_mode":"chatgpt","tokens":{"access_token":"host-native-secret","refresh_token":"native-refresh-secret","id_token":"native-id-secret","account_id":"account"}}`
			}
			source := filepath.Join(t.TempDir(), "credentials.json")
			if workspaceconfig.EnsurePrivateDir(filepath.Dir(source)) != nil {
				t.Fatal("private host credential fixture directory")
			}
			_ = os.WriteFile(source, []byte(document), 0600)
			e.Config.RuntimeAuth = map[string]workspaceconfig.RuntimeAuth{
				runtime: {Mode: "host-file", Source: source, Model: "native-model"},
			}
			e.Config.Endpoints = nil
			setMode(t, e, "echo-host-secret")
			result, err := e.Execute(context.Background(), runtime+".prompt", json.RawMessage(`{"prompt":"ok"}`))
			if err != nil || bytes.Contains(result, []byte("host-native-secret")) || !bytes.Contains(result, []byte("[redacted]")) {
				t.Fatal("native auth execution/redaction", err)
			}
			record := readRecord(t, e)
			if envValue(record.Env, "ANTHROPIC_API_KEY") != "" || envValue(record.Env, "ANTHROPIC_BASE_URL") != "" ||
				strings.Contains(strings.Join(record.Env, " "), "unrelated-developer-secret") {
				t.Fatal("native account reused a provider/global fallback")
			}
			if runtime == "claude" {
				if envValue(record.Env, "CLAUDE_CODE_OAUTH_TOKEN") != "host-native-secret" || !slicesContain(record.Args, "--bare") ||
					strings.Contains(strings.Join(record.Env, " "), "native-refresh-secret") {
					t.Fatal("Claude did not attach only the selected access token")
				}
			} else {
				if !strings.Contains(record.Files["auth.json"], "host-native-secret") ||
					!strings.Contains(record.Files["config.toml"], `model_provider = "openai"`) ||
					strings.Contains(record.Files["config.toml"], "gateway.example") {
					t.Fatal("Codex native account routed to custom provider")
				}
				if _, err := os.Stat(filepath.Join(envValue(record.Env, "CODEX_HOME"), "auth.json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("native credential snapshot retained after invocation")
				}
			}
			after, _ := os.ReadFile(source)
			if string(after) != document {
				t.Fatal("adapter modified developer's native account")
			}
			if _, err := e.Execute(context.Background(), runtime+".prompt", json.RawMessage(`{"prompt":"ok","endpoint":"local"}`)); !errors.Is(err, ErrInvalidConfig) {
				t.Fatal("native auth allowed endpoint routing", err)
			}
			_ = os.Chmod(source, 0644)
			if _, err := e.Execute(context.Background(), runtime+".prompt", json.RawMessage(`{"prompt":"ok"}`)); !errors.Is(err, ErrEndpointUnavailable) {
				t.Fatal("public native credential file accepted", err)
			}
		})
	}
}

func TestClaudeOAuthEnvRequiresExplicitNameAndBindsSession(t *testing.T) {
	e := fakeExecutor(t, "claude.prompt")
	t.Setenv("OWNER_APPROVED_OAUTH", "host-native-secret")
	e.Config.RuntimeAuth = map[string]workspaceconfig.RuntimeAuth{
		"claude": {Mode: "oauth-env", Source: "OWNER_APPROVED_OAUTH"},
	}
	e.Config.Endpoints = nil
	id, _ := sessionPrompt(t, e, "claude", "", "first")
	if envValue(readRecord(t, e).Env, "CLAUDE_CODE_OAUTH_TOKEN") != "host-native-secret" {
		t.Fatal("explicit oauth reference not resolved")
	}
	e.Config.RuntimeAuth["claude"] = workspaceconfig.RuntimeAuth{Mode: "oauth-env", Source: "CLAUDE_CODE_OAUTH_TOKEN"}
	raw, _ := json.Marshal(promptArgs{Prompt: "next", SessionID: id})
	if _, err := e.Execute(context.Background(), "claude.prompt", raw); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatal("resume changed selected account source", err)
	}
	e.Config.RuntimeAuth["claude"] = workspaceconfig.RuntimeAuth{Mode: "oauth-env", Source: "OWNER_APPROVED_UNSET"}
	if _, err := e.Execute(context.Background(), "claude.prompt", json.RawMessage(`{"prompt":"no"}`)); !errors.Is(err, ErrEndpointUnavailable) {
		t.Fatal("unset attachment silently reused global account", err)
	}
}
