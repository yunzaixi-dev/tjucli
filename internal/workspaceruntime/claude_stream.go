package workspaceruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
)

// Claude's stream-json SDK control wire is established by Anthropic's official
// claude-agent-sdk-python _internal/query.py, not inferred from display events.
func (e Executor) claudeStream(ctx context.Context, state runState, args []string, prompt string) (text string, err error) {
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd, err := e.command(childCtx, state, "claude", args)
	if err != nil {
		return "", err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", ErrRuntimeFailed
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return "", ErrRuntimeFailed
	}
	stderr := &cappedWriter{limit: maxStderr, cancel: cancel}
	cmd.Stderr = stderr
	if cmd.Start() != nil {
		stdin.Close()
		stdout.Close()
		return "", ErrRuntimeFailed
	}
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-childCtx.Done():
			stdin.Close()
			stdout.Close()
		case <-stopped:
		}
	}()
	defer func() {
		cancel()
		stdin.Close()
		stdout.Close()
		_ = cmd.Wait()
		if stderr.overflow() {
			text, err = "", ErrOutputLimit
		} else if ctx.Err() != nil {
			text, err = "", ErrCanceled
		}
	}()
	scanner := bufio.NewScanner(&budgetReader{reader: stdout, left: maxOutput, cancel: cancel})
	scanner.Buffer(make([]byte, 4096), maxOutput)
	writer := json.NewEncoder(stdin)
	if writer.Encode(map[string]any{"type": "control_request", "request_id": "initialize",
		"request": map[string]any{"subtype": "initialize", "hooks": nil}}) != nil {
		return "", ErrRuntimeFailed
	}
	initialized, denied := false, false
	seen := map[string]bool{}
	for scanner.Scan() {
		var event struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype string          `json:"subtype"`
				Tool    string          `json:"tool_name"`
				Input   json.RawMessage `json:"input"`
			} `json:"request"`
			Response struct {
				Subtype   string `json:"subtype"`
				RequestID string `json:"request_id"`
			} `json:"response"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			return "", ErrRuntimeFailed
		}
		switch event.Type {
		case "control_response":
			if initialized || event.Response.RequestID != "initialize" || event.Response.Subtype != "success" {
				return "", ErrRuntimeFailed
			}
			initialized = true
			if writer.Encode(map[string]any{"type": "user", "session_id": state.session.ID,
				"parent_tool_use_id": nil,
				"message":            map[string]any{"role": "user", "content": prompt}}) != nil {
				return "", ErrRuntimeFailed
			}
		case "control_request":
			if !initialized || event.RequestID == "" || len(event.RequestID) > 128 ||
				seen[event.RequestID] || len(seen) >= 256 {
				return "", ErrRuntimeFailed
			}
			seen[event.RequestID] = true
			response := map[string]any{"subtype": "error", "request_id": event.RequestID,
				"error": "unsupported host request"}
			if event.Request.Subtype == "can_use_tool" {
				decision := map[string]any{"behavior": "deny", "message": "Local owner approval required"}
				var input map[string]any
				if event.Request.Tool != "" && len(event.Request.Tool) <= 256 &&
					json.Unmarshal(event.Request.Input, &input) == nil && input != nil &&
					e.approve(ctx, state, "claude", "can_use_tool", event.Request.Tool, event.Request.Input) {
					decision = map[string]any{"behavior": "allow", "updatedInput": input}
				} else {
					denied = true
				}
				response = map[string]any{"subtype": "success", "request_id": event.RequestID, "response": decision}
			} else {
				denied = true
			}
			if writer.Encode(map[string]any{"type": "control_response", "response": response}) != nil {
				return "", ErrRuntimeFailed
			}
		case "result":
			if !initialized {
				return "", ErrRuntimeFailed
			}
			if denied {
				return "", ErrApprovalRequired
			}
			return claudeResult(scanner.Bytes())
		}
	}
	if errors.Is(scanner.Err(), ErrOutputLimit) || errors.Is(scanner.Err(), bufio.ErrTooLong) {
		return "", ErrOutputLimit
	}
	return "", ErrRuntimeFailed
}
