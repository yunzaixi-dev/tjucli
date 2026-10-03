package workspaceruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/yunzaixi-dev/tjucli/internal/workspacemcp"
)

func (e Executor) callMCP(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var input struct {
		Server    string          `json:"server"`
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if decodeObject(raw, &input, "server", "tool", "arguments") != nil ||
		input.Server == "" || len(input.Server) > 128 || strings.TrimSpace(input.Tool) == "" ||
		len(input.Tool) > 256 || strings.ContainsAny(input.Tool, "\x00\r\n") {
		return nil, ErrInvalidArguments
	}
	object := bytes.TrimSpace(input.Arguments)
	if len(object) == 0 || object[0] != '{' || !json.Valid(object) {
		return nil, ErrInvalidArguments
	}
	server, ok := e.Config.MCPServers[input.Server]
	if !ok {
		return nil, ErrCapabilityDenied
	}
	state, err := e.prepare()
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(state.runDir)
	// The helper owns stdio negotiation, configured argv/env, isolated MCP
	// state, and its own shorter deadline. It does not inherit process globals.
	result, err := workspacemcp.Call(ctx, e.StateDir, server, input.Tool, input.Arguments)
	if ctx.Err() != nil {
		return nil, ErrCanceled
	}
	if errors.Is(err, workspacemcp.ErrOutputLimit) {
		return nil, ErrOutputLimit
	}
	if err != nil {
		return nil, ErrRuntimeFailed
	}
	if len(result) > maxOutput || !json.Valid(result) {
		return nil, ErrOutputLimit
	}
	// Never forward a configured local environment value a server echoed.
	// Compare JSON-escaped values as well, so quotes/newlines cannot defeat it.
	for _, host := range server.Env {
		if value, ok := os.LookupEnv(host); ok && value != "" {
			encoded, _ := json.Marshal(value)
			if bytes.Contains(result, encoded[1:len(encoded)-1]) {
				return nil, ErrRuntimeFailed
			}
		}
	}
	return result, nil
}
