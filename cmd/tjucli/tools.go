package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/yunzaixi-dev/tjucli/internal/tjucli"
)

// Product tools (the user's notes, campus services, course materials, images)
// are served by the sandbox controller for the current Agent turn only. The
// controller sets TJUCLI_TOOLS_URL (loopback) and TJUCLI_TOOLS_TOKEN.

const (
	maxToolArguments = 60 << 10
	maxToolResponse  = 256 << 10
)

type toolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type toolCallResult struct {
	Name   string `json:"name"`
	Result any    `json:"result"`
}

const toolsHelp = `Usage:
  tjucli tools list [--json]
  tjucli tools call NAME [--args JSON|-] [--json]

Product tools for the current Agent turn: the user's notes (list_tree,
read_entry, create_entry, update_entry, delete_entry), campus services,
course materials and images. Run "tools list" for each tool's parameters.
--args takes a JSON object; "-" reads it from standard input.
`

func toolsEndpoint() (string, string, *tjucli.CLIError) {
	raw := strings.TrimRight(strings.TrimSpace(os.Getenv("TJUCLI_TOOLS_URL")), "/")
	token := strings.TrimSpace(os.Getenv("TJUCLI_TOOLS_TOKEN"))
	if raw == "" || token == "" {
		return "", "", tjucli.NewRuntimeError("tools_unavailable", "product tools are only available inside a TJUClaw Agent turn")
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Host == "" {
		return "", "", tjucli.NewRuntimeError("configuration_error", "invalid TJUCLI_TOOLS_URL")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return "", "", tjucli.NewRuntimeError("configuration_error", "TJUCLI_TOOLS_URL must be HTTPS or loopback HTTP")
	}
	return raw, token, nil
}

func (r *runner) runTools(ctx context.Context, args []string, jsonOutput bool) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(r.stdout, toolsHelp)
		return 0
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return r.fail(jsonOutput, tjucli.NewFlagError("tools list does not accept arguments"))
		}
		var reply struct {
			Tools []struct {
				Function toolSpec `json:"function"`
			} `json:"tools"`
		}
		if err := r.toolsRequest(ctx, http.MethodGet, "/v1/tools", nil, &reply); err != nil {
			return r.fail(jsonOutput, err)
		}
		specs := make([]toolSpec, 0, len(reply.Tools))
		for _, tool := range reply.Tools {
			specs = append(specs, tool.Function)
		}
		if jsonOutput {
			return r.success(map[string]any{"tools": specs}, struct{}{})
		}
		for _, spec := range specs {
			fmt.Fprintf(r.stdout, "%s\t%s\n", spec.Name, spec.Description)
		}
		return 0
	case "call":
		positional, options, parseErr := parseOptions(args[1:], map[string]bool{"args": true})
		if parseErr != nil {
			return r.fail(jsonOutput, parseErr)
		}
		if len(positional) != 1 || strings.TrimSpace(positional[0]) == "" {
			return r.fail(jsonOutput, tjucli.NewFlagError("tools call requires exactly one tool name"))
		}
		arguments := options["args"]
		if arguments == "-" {
			data, err := io.ReadAll(io.LimitReader(os.Stdin, maxToolArguments+1))
			if err != nil || len(data) > maxToolArguments {
				return r.fail(jsonOutput, tjucli.NewFlagError("--args from standard input is unreadable or too large"))
			}
			arguments = string(data)
		}
		if strings.TrimSpace(arguments) == "" {
			arguments = "{}"
		}
		var object map[string]any
		if len(arguments) > maxToolArguments || json.Unmarshal([]byte(arguments), &object) != nil || object == nil {
			return r.fail(jsonOutput, tjucli.NewFlagError("--args must be a JSON object"))
		}
		payload, _ := json.Marshal(map[string]any{"name": positional[0], "arguments": json.RawMessage(arguments)})
		var reply struct {
			Name   string `json:"name"`
			Result string `json:"result"`
		}
		if err := r.toolsRequest(ctx, http.MethodPost, "/v1/tools/call", payload, &reply); err != nil {
			return r.fail(jsonOutput, err)
		}
		// Tool results are JSON text; hand them over structured when they are.
		var result any = reply.Result
		var parsed any
		if json.Unmarshal([]byte(reply.Result), &parsed) == nil {
			result = parsed
		}
		if jsonOutput {
			return r.success(toolCallResult{Name: reply.Name, Result: result}, struct{}{})
		}
		fmt.Fprintln(r.stdout, reply.Result)
		return 0
	default:
		return r.fail(jsonOutput, tjucli.NewFlagError(fmt.Sprintf("unknown tools command %q", args[0])))
	}
}

func (r *runner) toolsRequest(ctx context.Context, method, path string, body []byte, dest any) *tjucli.CLIError {
	base, token, cliErr := toolsEndpoint()
	if cliErr != nil {
		return cliErr
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return tjucli.NewRuntimeError("tools_unavailable", "could not build the tool request")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return tjucli.NewRuntimeError("tools_unavailable", "the tool service did not respond")
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxToolResponse+1))
	if err != nil || len(data) > maxToolResponse {
		return tjucli.NewRuntimeError("upstream_error", "the tool response was unreadable or too large")
	}
	switch {
	case res.StatusCode == http.StatusNotFound:
		return tjucli.NewRuntimeError("unknown_tool", "no such tool; run 'tjucli tools list'")
	case res.StatusCode == http.StatusUnauthorized:
		return tjucli.NewRuntimeError("tool_grant_invalid", "the tool grant for this turn is no longer valid")
	case res.StatusCode == http.StatusBadRequest:
		return tjucli.NewFlagError("the tool rejected the request")
	case res.StatusCode != http.StatusOK:
		return tjucli.NewRuntimeError("tools_unavailable", fmt.Sprintf("the tool service returned HTTP %d", res.StatusCode))
	}
	if json.Unmarshal(data, dest) != nil {
		return tjucli.NewRuntimeError("protocol_error", "the tool service returned malformed JSON")
	}
	return nil
}
