package workspaceruntime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/yunzaixi-dev/tjucli/internal/workspaceconfig"
)

// This documented API enables every configured built-in tool (including the
// optional grep/find/ls), retaining rather than force-enabling plugin tools.
// Loaded before owner plugins, so their permission hooks/policy still apply.
const piHostToolsSource = `export default function hostTools(pi) {
  pi.on("session_start", () => {
    const builtin = pi.getAllTools().filter(t => t.sourceInfo?.source === "builtin").map(t => t.name);
    pi.setActiveTools([...new Set([...pi.getActiveTools(), ...builtin])]);
  });
}
`

func (e Executor) pi(ctx context.Context, state runState, endpoint workspaceconfig.Endpoint, prompt string) (string, error) {
	agentDir := filepath.Join(state.runDir, "pi")
	models := map[string]any{"providers": map[string]any{
		"workspace": map[string]any{
			"baseUrl": endpoint.BaseURL, "api": "openai-completions",
			"apiKey": "$" + keyVariable,
			"models": []any{map[string]any{"id": endpoint.Model, "input": []string{"text"}}},
		},
	}}
	if endpoint.APIKeyEnv == "" {
		// Pi requires an auth marker even for a keyless local provider. This
		// public placeholder is not an inherited account credential.
		models["providers"].(map[string]any)["workspace"].(map[string]any)["apiKey"] = "workspace-keyless"
	}
	if privateJSON(filepath.Join(agentDir, "models.json"), models) != nil ||
		privateJSON(filepath.Join(agentDir, "settings.json"), map[string]any{
			"defaultProjectTrust": "never", "packages": []string{},
		}) != nil {
		return "", ErrInvalidConfig
	}
	state.env = append(state.env, "PI_CODING_AGENT_DIR="+agentDir,
		"PI_CODING_AGENT_SESSION_DIR="+state.sessions, "PI_OFFLINE=1", "PI_TELEMETRY=0")
	// The local owner's explicit pi.prompt grant authorizes full host tools.
	// Pi has no built-in tool approval/sandbox gate. Do not misrepresent
	// project resource trust (--no-approve) as a host permission boundary.
	args := []string{"--print", "--mode", "json", "--provider", "workspace", "--model", endpoint.Model,
		"--session-dir", state.sessions, "--no-approve", "--no-extensions",
		"--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files"}
	if state.session != nil {
		sessionFile := filepath.Join(state.session.dir, "pi", "session.jsonl")
		if state.resumed {
			if _, err := readPrivateFile(sessionFile, maxHistory); err != nil {
				return "", ErrSessionUnavailable
			}
		} else {
			// Pi initializes a valid header when its explicit session file is
			// empty. Precreate it privately instead of relying on host umask.
			if privateBytes(sessionFile, nil) != nil {
				return "", ErrInvalidConfig
			}
		}
		args = append(args, "--session", sessionFile)
	}
	hostTools := filepath.Join(agentDir, "host-tools.ts")
	if privateBytes(hostTools, []byte(piHostToolsSource)) != nil {
		return "", ErrInvalidConfig
	}
	args = append(args, "--extension", hostTools)
	// Plugins are local-owner approved extension paths, not remote argv,
	// package URLs or auto-installed/global extensions. Their code itself is
	// trusted host code, just as Pi's built-in shell tools have host access.
	for _, plugin := range e.Config.Plugins {
		if !filepath.IsAbs(plugin) || len(plugin) > 4096 || strings.ContainsRune(plugin, 0) {
			return "", ErrInvalidConfig
		}
		args = append(args, "--extension", plugin)
	}
	if e.piLinked() || len(e.piMCPReferences()) != 0 {
		extension, err := e.piBridge(state)
		if err != nil {
			return "", err
		}
		args = append(args, "--extension", extension)
	}
	// stdin avoids both flag injection and Pi's @file positional expansion.
	output, err := e.printProcess(ctx, state, "pi", args, prompt)
	if err != nil {
		return "", err
	}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 4096), maxOutput)
	var text strings.Builder
	found, completed := false, false
	for scanner.Scan() {
		var event struct {
			Type    string `json:"type"`
			Message struct {
				Role       string `json:"role"`
				StopReason string `json:"stopReason"`
				Content    []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			return "", ErrRuntimeFailed
		}
		if event.Type == "agent_end" {
			completed = true
		}
		if event.Type == "message_end" && event.Message.Role == "assistant" {
			if event.Message.StopReason == "error" || event.Message.StopReason == "aborted" {
				return "", ErrRuntimeFailed
			}
			found = true
			for _, block := range event.Message.Content {
				if block.Type == "text" {
					text.WriteString(block.Text)
				}
			}
		}
	}
	if scanner.Err() != nil || !found || !completed {
		return "", ErrRuntimeFailed
	}
	return text.String(), nil
}
