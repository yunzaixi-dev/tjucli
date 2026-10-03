package workspaceruntime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/yunzaixi-dev/tjucli/internal/workspaceconfig"
)

// HostAuth is an explicit LOCAL owner attachment, not global environment
// inheritance. Source is an absolute credential file, or a named env reference.
// No HOME/keychain scanning, login flow, account fallback or source writeback.
type HostAuth = workspaceconfig.RuntimeAuth

func (e Executor) runtimeEndpoint(runtime, name string) (workspaceconfig.Endpoint, string, HostAuth, error) {
	auth, ok := e.Config.RuntimeAuth[runtime]
	if !ok {
		endpoint, key, err := e.endpoint(name)
		return endpoint, key, HostAuth{}, err
	}
	if (runtime != "claude" && runtime != "codex") || name != "" ||
		len(auth.Model) > 256 || strings.ContainsAny(auth.Model, "\x00\r\n") ||
		(auth.Mode != "host-file" && (runtime != "claude" || auth.Mode != "oauth-env")) {
		return workspaceconfig.Endpoint{}, "", auth, ErrInvalidConfig
	}
	if auth.Mode == "host-file" {
		canonical, err := filepath.EvalSymlinks(auth.Source)
		if !filepath.IsAbs(auth.Source) || err != nil || canonical != filepath.Clean(auth.Source) {
			return workspaceconfig.Endpoint{}, "", auth, ErrEndpointUnavailable
		}
	} else if !envName.MatchString(auth.Source) {
		return workspaceconfig.Endpoint{}, "", auth, ErrInvalidConfig
	}
	// Host account tokens must never be routed to a caller/config endpoint.
	return workspaceconfig.Endpoint{Name: "host-login", Model: auth.Model}, "", auth, nil
}

func secretStrings(value any, result *[]string) {
	switch v := value.(type) {
	case string:
		if v != "" {
			*result = append(*result, v)
		}
	case map[string]any:
		for _, child := range v {
			secretStrings(child, result)
		}
	case []any:
		for _, child := range v {
			secretStrings(child, result)
		}
	}
}

func (e Executor) attachAuth(state *runState, runtime string, auth HostAuth) (func(), error) {
	state.auth = auth
	if auth.Mode == "" {
		return func() {}, nil
	}
	if auth.Mode == "oauth-env" {
		value, ok := os.LookupEnv(auth.Source)
		if !ok || value == "" || len(value) > 8192 || strings.ContainsAny(value, "\x00\r\n") {
			return nil, ErrEndpointUnavailable
		}
		state.env = append(state.env, "CLAUDE_CODE_OAUTH_TOKEN="+value)
		state.secrets = append(state.secrets, value)
		return func() {}, nil
	}
	data, err := readPrivateFile(auth.Source, maxOutput)
	var document map[string]any
	if err != nil || json.Unmarshal(data, &document) != nil || document == nil {
		return nil, ErrEndpointUnavailable
	}
	if runtime == "codex" {
		tokens, _ := document["tokens"].(map[string]any)
		token, _ := tokens["access_token"].(string)
		key, _ := document["OPENAI_API_KEY"].(string)
		if token == "" && key == "" {
			return nil, ErrEndpointUnavailable
		}
	} else {
		oauth, _ := document["claudeAiOauth"].(map[string]any)
		token, _ := oauth["accessToken"].(string)
		if token == "" || len(token) > 8192 || strings.ContainsAny(token, "\x00\r\n") {
			return nil, ErrEndpointUnavailable
		}
		// Use only the selected native account's access token. Explicit env
		// auth plus --bare avoids ambient keychain lookup and source refresh/
		// mutation. The owner refreshes the native login outside this adapter.
		secretStrings(document, &state.secrets)
		state.env = append(state.env, "CLAUDE_CODE_OAUTH_TOKEN="+token)
		return func() {}, nil
	}
	secretStrings(document, &state.secrets)
	filename := "auth.json"
	destination := filepath.Join(state.session.dir, runtime, filename)
	// Copy exactly one owner-selected file, never settings, extensions, MCP,
	// native history or ~/.claude.json. Native refresh may update ONLY this
	// workspace copy; the developer's source account file is never modified.
	if info, err := os.Lstat(destination); err == nil && !info.Mode().IsRegular() {
		return nil, ErrInvalidConfig
	}
	if privateBytes(destination, data) != nil {
		return nil, ErrInvalidConfig
	}
	return func() { _ = os.Remove(destination) }, nil
}
