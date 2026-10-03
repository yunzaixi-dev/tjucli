// Package workspaceruntime runs explicitly enabled, locally configured host
// runtimes. Remote arguments never select an executable, cwd, or credentials.
package workspaceruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yunzaixi-dev/tjucli/internal/workspaceconfig"
)

var (
	ErrCapabilityDenied    = errors.New("workspace_capability_denied")
	ErrInvalidArguments    = errors.New("workspace_invalid_arguments")
	ErrInvalidConfig       = errors.New("workspace_runtime_config_invalid")
	ErrEndpointRequired    = errors.New("workspace_endpoint_required")
	ErrEndpointUnavailable = errors.New("workspace_endpoint_unavailable")
	ErrRuntimeUnavailable  = errors.New("workspace_runtime_unavailable")
	ErrRuntimeFailed       = errors.New("workspace_runtime_failed")
	ErrOutputLimit         = errors.New("workspace_runtime_output_limit")
	ErrCanceled            = errors.New("workspace_runtime_canceled")
	ErrApprovalRequired    = errors.New("workspace_runtime_approval_required")
)

const (
	maxArguments = 64 << 10
	maxPrompt    = 32 << 10
	maxOutput    = 1 << 20
	maxStderr    = 64 << 10
	runTimeout   = 30 * time.Minute
	keyVariable  = "TJUCLAW_RUNTIME_MODEL_KEY"
)

// Executor's StateDir is the absolute, private workspace configuration
// directory. Root is only the execution context, never the workspace identity.
type Executor struct {
	Config   workspaceconfig.Config
	StateDir string
	// Local owner settings, never populated from remote prompt arguments.
	Approvals ApprovalHandler
	// Owner-authored local ceiling, never a prompt field. Zero uses 30m.
	RunTimeout time.Duration
}

type promptArgs struct {
	Prompt    string `json:"prompt"`
	Endpoint  string `json:"endpoint,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// Execute rechecks local permission before even parsing untrusted arguments.
// It returns final assistant text, not raw process events or diagnostics.
func (e Executor) Execute(ctx context.Context, capability string, args json.RawMessage) (json.RawMessage, error) {
	if !slices.Contains(e.Config.AllowedCapabilities, capability) ||
		!slices.Contains([]string{"pi.prompt", "claude.prompt", "codex.prompt", "mcp.call"}, capability) {
		return nil, ErrCapabilityDenied
	}
	if ctx == nil {
		return nil, ErrInvalidArguments
	}
	if ctx.Err() != nil {
		return nil, ErrCanceled
	}
	// A daemon-wide Windows Job is not per-invocation containment: timeout
	// must terminate this invocation's descendants without stopping others.
	// Reject unsupported prompt execution before inspecting credentials or
	// creating runtime state/conversations. Local metadata APIs are separate.
	if capability != "mcp.call" && !promptProcessTreeSupported {
		return nil, ErrRuntimeUnavailable
	}
	if workspaceconfig.Validate(e.Config) != nil {
		return nil, ErrInvalidConfig
	}
	timeout := e.RunTimeout
	if timeout == 0 {
		timeout = runTimeout
	}
	if timeout < 0 || timeout > 24*time.Hour {
		return nil, ErrInvalidConfig
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if capability == "mcp.call" {
		return e.callMCP(ctx, args)
	}
	var input promptArgs
	if decodeObject(args, &input, "prompt", "endpoint", "session_id") != nil ||
		strings.TrimSpace(input.Prompt) == "" || len(input.Prompt) > maxPrompt ||
		!utf8.ValidString(input.Prompt) || strings.ContainsRune(input.Prompt, 0) ||
		len(input.Endpoint) > 128 || (input.SessionID != "" && !sessionIDPattern.MatchString(input.SessionID)) {
		return nil, ErrInvalidArguments
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(args, &fields)
	if _, supplied := fields["session_id"]; supplied && input.SessionID == "" {
		return nil, ErrInvalidArguments
	}
	runtime := strings.TrimSuffix(capability, ".prompt")
	endpoint, key, auth, err := e.runtimeEndpoint(runtime, input.Endpoint)
	if err != nil {
		return nil, err
	}
	state, err := e.prepare()
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(state.runDir)
	state.env = append(state.env, keyVariable+"="+key)
	session, unlock, err := e.openConversation(ctx, runtime, input.SessionID, endpoint.Name, bindingHash(endpoint, auth))
	if err != nil {
		return nil, err
	}
	defer unlock()
	state.session, state.resumed = session, input.SessionID != ""
	success := false
	defer func() {
		if !success {
			_ = session.finish(false)
		}
	}()
	cleanup, err := e.attachAuth(&state, runtime, auth)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	var text string
	switch capability {
	case "pi.prompt":
		text, err = e.pi(ctx, state, endpoint, input.Prompt)
	case "claude.prompt":
		text, err = e.claude(ctx, state, endpoint, input.Prompt, key)
	case "codex.prompt":
		text, err = e.codex(ctx, state, endpoint, input.Prompt)
	}
	if err != nil {
		return nil, err
	}
	if err := session.checkFiles(); err != nil {
		return nil, err
	}
	// A misbehaving model/process must not echo the selected credential or
	// full configured gateway URL into the remote completion result.
	if key != "" {
		text = strings.ReplaceAll(text, key, "[redacted]")
	}
	// Pi's MCP bridge keeps credentials out of model-facing results. Also
	// redact an approved MCP value if a model/process echoes it in final text.
	if capability == "pi.prompt" {
		for _, hosts := range e.piMCPReferences() {
			for _, host := range hosts {
				if value := os.Getenv(host); value != "" {
					text = strings.ReplaceAll(text, value, "[redacted]")
				}
			}
		}
	}
	for _, secret := range state.secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	if endpoint.BaseURL != "" {
		text = strings.ReplaceAll(text, endpoint.BaseURL, "[endpoint]")
	}
	data, err := json.Marshal(struct {
		Runtime   string `json:"runtime"`
		Output    string `json:"output"`
		SessionID string `json:"session_id"`
	}{runtime, text, session.ID})
	if err != nil || len(data) > maxOutput {
		return nil, ErrOutputLimit
	}
	if session.finish(true) != nil {
		return nil, ErrInvalidConfig
	}
	success = true
	return data, nil
}

// decodeObject rejects unknown/duplicate fields, nulls, trailing values, and
// non-object arguments, rather than silently accepting future command knobs.
func decodeObject(raw json.RawMessage, out any, fields ...string) error {
	if len(raw) == 0 || len(raw) > maxArguments || !utf8.Valid(raw) {
		return ErrInvalidArguments
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return ErrInvalidArguments
	}
	seen := map[string]bool{}
	for dec.More() {
		token, err = dec.Token()
		name, ok := token.(string)
		if err != nil || !ok || !slices.Contains(fields, name) || seen[name] {
			return ErrInvalidArguments
		}
		seen[name] = true
		var value json.RawMessage
		if dec.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return ErrInvalidArguments
		}
	}
	if _, err = dec.Token(); err != nil {
		return ErrInvalidArguments
	}
	if dec.Decode(new(any)) != io.EOF || json.Unmarshal(raw, out) != nil {
		return ErrInvalidArguments
	}
	return nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

func (e Executor) endpoint(name string) (workspaceconfig.Endpoint, string, error) {
	if name == "" {
		if len(e.Config.Endpoints) != 1 {
			return workspaceconfig.Endpoint{}, "", ErrEndpointRequired
		}
		name = e.Config.Endpoints[0].Name
	}
	var selected workspaceconfig.Endpoint
	found := false
	for _, candidate := range e.Config.Endpoints {
		if candidate.Name == name {
			if found {
				return selected, "", ErrInvalidConfig
			}
			selected, found = candidate, true
		}
	}
	if !found {
		return selected, "", ErrEndpointUnavailable
	}
	u, err := url.Parse(selected.BaseURL)
	if err != nil || len(selected.BaseURL) > 2048 || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "https" && u.Scheme != "http") ||
		len(selected.Model) == 0 || len(selected.Model) > 256 ||
		strings.ContainsAny(selected.Model, "\x00\r\n") ||
		(selected.APIKeyEnv != "" && !envName.MatchString(selected.APIKeyEnv)) {
		return selected, "", ErrInvalidConfig
	}
	if selected.APIKeyEnv == "" {
		return selected, "", nil
	}
	key, ok := os.LookupEnv(selected.APIKeyEnv)
	if !ok || strings.TrimSpace(key) == "" || len(key) > 8192 || strings.ContainsAny(key, "\x00\r\n") {
		return selected, "", ErrEndpointUnavailable
	}
	return selected, key, nil
}

type runState struct {
	runDir   string
	sessions string
	env      []string
	session  *conversation
	resumed  bool
	auth     HostAuth
	secrets  []string
}

func (e Executor) prepare() (runState, error) {
	var state runState
	root, err := os.Stat(e.Config.Root)
	if !filepath.IsAbs(e.Config.Root) || err != nil || !root.IsDir() ||
		!filepath.IsAbs(e.StateDir) {
		return state, ErrInvalidConfig
	}
	// StateDir is supplied by the local owner, not from remote JSON. Reject
	// redirected state paths to avoid loading another workspace's accounts.
	canonical, err := filepath.EvalSymlinks(e.StateDir)
	if err != nil || canonical != filepath.Clean(e.StateDir) {
		return state, ErrInvalidConfig
	}
	base := filepath.Join(e.StateDir, "runtime")
	for _, dir := range []string{base, filepath.Join(base, "runs"), filepath.Join(base, "pi"), filepath.Join(base, "pi", "sessions")} {
		if privateDir(dir) != nil {
			return state, ErrInvalidConfig
		}
	}
	state.runDir, err = os.MkdirTemp(filepath.Join(base, "runs"), "run-")
	if err != nil {
		return state, ErrInvalidConfig
	}
	if privateDir(state.runDir) != nil {
		os.RemoveAll(state.runDir)
		return runState{}, ErrInvalidConfig
	}
	state.sessions = filepath.Join(base, "pi", "sessions")
	for _, name := range []string{"home", "tmp", "config", "cache", "data", "pi", "codex", "claude"} {
		if privateDir(filepath.Join(state.runDir, name)) != nil {
			os.RemoveAll(state.runDir)
			return runState{}, ErrInvalidConfig
		}
	}
	// No inherited provider keys, OAuth tokens, proxies, NODE_OPTIONS, shell
	// hooks, global config overrides, or parent agent session markers.
	state.env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(state.runDir, "home"),
		"USERPROFILE=" + filepath.Join(state.runDir, "home"),
		"TMPDIR=" + filepath.Join(state.runDir, "tmp"),
		"TMP=" + filepath.Join(state.runDir, "tmp"),
		"TEMP=" + filepath.Join(state.runDir, "tmp"),
		"XDG_CONFIG_HOME=" + filepath.Join(state.runDir, "config"),
		"XDG_CACHE_HOME=" + filepath.Join(state.runDir, "cache"),
		"XDG_DATA_HOME=" + filepath.Join(state.runDir, "data"),
		"LANG=C.UTF-8", "TERM=dumb", "NO_COLOR=1",
	}
	if systemRoot := os.Getenv("SystemRoot"); systemRoot != "" {
		state.env = append(state.env, "SystemRoot="+systemRoot)
	}
	return state, nil
}

func privateDir(path string) error {
	if workspaceconfig.EnsurePrivateDir(path) != nil {
		return ErrInvalidConfig
	}
	return nil
}

func privateJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return ErrInvalidConfig
	}
	return privateBytes(path, data)
}
