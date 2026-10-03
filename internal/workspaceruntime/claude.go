package workspaceruntime

import (
	"context"
	"encoding/json"
	"path/filepath"

	"github.com/yunzaixi-dev/tjucli/internal/workspaceconfig"
)

func (e Executor) claude(ctx context.Context, state runState, endpoint workspaceconfig.Endpoint, prompt, key string) (string, error) {
	if key == "" {
		key = "workspace-keyless"
	}
	home := filepath.Join(state.session.dir, "claude")
	state.env = append(state.env, "CLAUDE_CONFIG_DIR="+home,
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL=1")
	if state.auth.Mode == "" {
		state.env = append(state.env, "ANTHROPIC_API_KEY="+key, "ANTHROPIC_BASE_URL="+endpoint.BaseURL)
	} else {
		state.env = append(state.env, "CLAUDE_CODE_SAFE_MODE=1")
	}
	// No inherited hooks, plugins, MCP, account login, or project permission
	// rules. Print mode cannot obtain local human approval: deny requests.
	args := []string{"--print", "--output-format", "json", "--bare",
		"--permission-mode", "manual", "--permission-prompts", "none",
		"--setting-sources", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
	}
	if state.auth.Mode != "" {
		args = append(args, "--safe-mode")
	}
	if endpoint.Model != "" {
		args = append(args, "--model", endpoint.Model)
	}
	if state.resumed {
		args = append(args, "--resume="+state.session.ID)
	} else {
		args = append(args, "--session-id="+state.session.ID)
	}
	if e.Approvals != nil {
		for i := range args {
			if args[i] == "json" {
				args[i] = "stream-json"
			} else if args[i] == "none" {
				args[i] = "host"
			}
		}
		args = append(args, "--input-format", "stream-json", "--verbose")
		return e.claudeStream(ctx, state, args, prompt)
	}
	output, err := e.printProcess(ctx, state, "claude", args, prompt)
	if err != nil {
		return "", err
	}
	return claudeResult(output)
}

func claudeResult(output []byte) (string, error) {
	var result struct {
		Type              string            `json:"type"`
		Subtype           string            `json:"subtype"`
		IsError           bool              `json:"is_error"`
		Result            *string           `json:"result"`
		PermissionDenials []json.RawMessage `json:"permission_denials"`
	}
	if json.Unmarshal(output, &result) != nil || result.Type != "result" {
		return "", ErrRuntimeFailed
	}
	if len(result.PermissionDenials) != 0 {
		return "", ErrApprovalRequired
	}
	if result.IsError || result.Subtype != "success" || result.Result == nil {
		return "", ErrRuntimeFailed
	}
	return *result.Result, nil
}
