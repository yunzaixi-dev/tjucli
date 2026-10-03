package workspacemcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yunzaixi-dev/tjucli/internal/workspaceconfig"
)

func helperServer(t *testing.T, mode string) workspaceconfig.MCPServer {
	t.Helper()
	if !processTreeSupported {
		t.Skip("MCP execution intentionally fails closed without a process-tree supervisor")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOST_MCP_HELPER_MODE", mode)
	return workspaceconfig.MCPServer{
		Command: executable,
		Args:    []string{"-test.run=^TestMCPHelperProcess$"},
		Env:     map[string]string{"WORKSPACE_MCP_HELPER": "HOST_MCP_HELPER_MODE"},
	}
}

func TestCallUnsupportedSupervisionFailsBeforeSideEffects(t *testing.T) {
	if processTreeSupported {
		t.Skip("platform has a process-group supervisor")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "not-created")
	server := workspaceconfig.MCPServer{
		Command: executable,
		Args:    []string{"-test.run=^TestMCPHelperProcess$"},
		// Deliberately unset: unsupported supervision must be checked even
		// before reading owner-approved credential references.
		Env: map[string]string{"KEY": "TJUCLAW_MCP_UNSET_SUPERVISION_TEST"},
	}
	_, err = Call(context.Background(), dir, server, "echo", json.RawMessage(`{}`))
	if !errors.Is(err, ErrSupervisionUnsupported) {
		t.Fatalf("unsupported platform did not fail closed: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unsupported supervision created state")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Call(ctx, dir, server, "echo", json.RawMessage(`{}`)); !errors.Is(err, context.Canceled) {
		t.Fatal("pre-cancellation must take precedence over unsupported supervision")
	}
}

// TestMCPHelperProcess is executed only by isolated child test binaries.
// Failure exits use private diagnostic codes, never a real MCP/model server.
func TestMCPHelperProcess(t *testing.T) {
	mode := os.Getenv("WORKSPACE_MCP_HELPER")
	if mode == "" {
		return
	}
	switch mode {
	case "hang":
		time.Sleep(time.Minute)
		os.Exit(0)
	case "stderr-limit":
		_, _ = os.Stderr.Write([]byte(strings.Repeat("private-value", MaxStderrBytes)))
		time.Sleep(time.Minute)
		os.Exit(0)
	case "message-limit":
		_, _ = os.Stdout.Write([]byte(strings.Repeat("x", MaxMessageBytes+20) + "\n"))
		os.Exit(0)
	case "notifications-limit":
		for range MaxMessages + 1 {
			fmt.Println(`{"jsonrpc":"2.0","method":"notifications/progress","params":{}}`)
		}
		os.Exit(0)
	case "output-limit":
		for range 6 {
			fmt.Printf("{\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"x\":%q}}\n", strings.Repeat("x", MaxMessageBytes-128))
		}
		os.Exit(0)
	case "invalid-json":
		fmt.Println("private-unparseable-server-output")
		os.Exit(0)
	case "empty":
		os.Exit(0)
	}
	reader := bufio.NewScanner(os.Stdin)
	read := func() map[string]json.RawMessage {
		if !reader.Scan() {
			os.Exit(11)
		}
		var msg map[string]json.RawMessage
		if json.Unmarshal(reader.Bytes(), &msg) != nil || string(msg["jsonrpc"]) != `"2.0"` {
			os.Exit(12)
		}
		return msg
	}
	write := func(value any) {
		if err := json.NewEncoder(os.Stdout).Encode(value); err != nil {
			os.Exit(13)
		}
	}
	initialize := read()
	if string(initialize["method"]) != `"initialize"` || string(initialize["id"]) != "1" {
		os.Exit(14)
	}
	var params struct {
		Version      string         `json:"protocolVersion"`
		Capabilities map[string]any `json:"capabilities"`
		ClientInfo   map[string]any `json:"clientInfo"`
	}
	if json.Unmarshal(initialize["params"], &params) != nil || params.Version != ProtocolVersion || params.Capabilities == nil || params.ClientInfo["name"] != "tjuclaw" {
		os.Exit(15)
	}
	if mode == "rpc-error" {
		write(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -1, "message": "private-error-message"}})
		os.Exit(0)
	}
	if mode == "wrong-id" {
		write(map[string]any{"jsonrpc": "2.0", "id": 9, "result": map[string]any{}})
		os.Exit(0)
	}
	if mode == "server-request" {
		write(map[string]any{"jsonrpc": "2.0", "id": "server-1", "method": "sampling/createMessage", "params": map[string]any{}})
		response := read()
		if string(response["id"]) != `"server-1"` || len(response["error"]) == 0 {
			os.Exit(16)
		}
	}
	version := ProtocolVersion
	if mode == "unsupported-version" {
		version = "2099-01-01"
	}
	if mode == "old-version" {
		version = "2024-11-05"
	}
	if mode == "malformed-initialize" {
		write(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"protocolVersion": version}})
		os.Exit(0)
	}
	write(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{
		"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}},
		"serverInfo": map[string]any{"name": "fake", "version": "1"},
	}})
	initialized := read()
	if string(initialized["method"]) != `"notifications/initialized"` || len(initialized["id"]) != 0 {
		os.Exit(17)
	}
	call := read()
	if string(call["method"]) != `"tools/call"` || string(call["id"]) != "2" {
		os.Exit(18)
	}
	var callParams struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if json.Unmarshal(call["params"], &callParams) != nil || callParams.Name != "echo" || callParams.Arguments["value"] != "hello" {
		os.Exit(19)
	}
	if mode == "hang-call" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	if mode == "tool-error" {
		write(map[string]any{"jsonrpc": "2.0", "id": 2, "error": map[string]any{"code": -1, "message": "private-tool-error-message"}})
		os.Exit(0)
	}
	if mode == "array-result" {
		write(map[string]any{"jsonrpc": "2.0", "id": 2, "result": []string{"bad"}})
		os.Exit(0)
	}
	if mode == "notifications" {
		fmt.Println(`{"jsonrpc":"2.0","method":"notifications/progress","params":{}}`)
	}
	result := map[string]any{"content": []any{map[string]any{"type": "text", "text": "hello"}}}
	if mode == "environment" {
		cwd, _ := os.Getwd()
		result["environment"] = map[string]any{
			"cwd": cwd, "home": os.Getenv("HOME"), "config": os.Getenv("XDG_CONFIG_HOME"),
			"tmp": os.Getenv("TMPDIR"), "secret": os.Getenv("API_KEY"),
			"ambient": os.Getenv("AMBIENT_MODEL_SECRET"), "pi": os.Getenv("PI_CODING_AGENT_DIR"),
			"literal": os.Getenv("LITERAL_VALUE"),
		}
	}
	write(map[string]any{"jsonrpc": "2.0", "id": 2, "result": result})
	// Do not exit before the caller has consumed stdout. A pipe closure is the
	// normal indication that the one-shot client is finished.
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func TestCallNegotiatesAndInvokesConfiguredTool(t *testing.T) {
	for _, mode := range []string{"success", "notifications", "server-request", "old-version"} {
		t.Run(mode, func(t *testing.T) {
			server := helperServer(t, mode)
			result, err := Call(context.Background(), t.TempDir(), server, "echo", json.RawMessage(`{"value":"hello"}`))
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(result, &decoded) != nil || len(decoded.Content) != 1 || decoded.Content[0].Text != "hello" {
				t.Fatalf("unexpected tool result: %s", result)
			}
		})
	}
}

func TestCallIsolationAndExplicitEnvReferences(t *testing.T) {
	server := helperServer(t, "environment")
	t.Setenv("AMBIENT_MODEL_SECRET", "must-not-be-inherited")
	t.Setenv("PI_CODING_AGENT_DIR", "/developer/global/pi")
	t.Setenv("HOST_PRIVATE_MCP_KEY", "private-test-secret")
	server.Env["API_KEY"] = "HOST_PRIVATE_MCP_KEY"
	base := t.TempDir()
	dirs := []string{filepath.Join(base, "one"), filepath.Join(base, "two")}
	homes := []string{}
	for _, dir := range dirs {
		result, err := Call(context.Background(), dir, server, "echo", json.RawMessage(`{"value":"hello"}`))
		if err != nil {
			t.Fatal(err)
		}
		var decoded struct {
			Environment map[string]string `json:"environment"`
		}
		if err := json.Unmarshal(result, &decoded); err != nil {
			t.Fatal(err)
		}
		env := decoded.Environment
		if env["secret"] != "private-test-secret" || env["ambient"] != "" || env["pi"] != "" || env["literal"] != "" {
			t.Fatal("secret resolution or ambient isolation failed")
		}
		for _, key := range []string{"cwd", "home", "config", "tmp"} {
			rel, err := filepath.Rel(dir, env[key])
			if err != nil || !filepath.IsLocal(rel) {
				t.Fatalf("%s escapes independent environment state", key)
			}
			if runtime.GOOS != "windows" {
				info, err := os.Stat(env[key])
				if err != nil || info.Mode().Perm() != 0700 {
					t.Fatalf("%s is not a private directory", key)
				}
			}
		}
		homes = append(homes, env["home"])
	}
	if homes[0] == homes[1] {
		t.Fatal("MCP servers shared state between environment instances")
	}
}

func TestCallRejectsProtocolErrorsAndBounds(t *testing.T) {
	for _, mode := range []string{
		"rpc-error", "tool-error", "wrong-id", "unsupported-version", "malformed-initialize",
		"array-result", "invalid-json", "empty", "message-limit", "notifications-limit", "output-limit", "stderr-limit",
	} {
		t.Run(mode, func(t *testing.T) {
			server := helperServer(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := Call(ctx, t.TempDir(), server, "echo", json.RawMessage(`{"value":"hello"}`))
			if err == nil {
				t.Fatal("unsafe protocol response was accepted")
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("limits must interrupt the process before timeout")
			}
			if strings.Contains(err.Error(), "private-") {
				t.Fatal("server diagnostics leaked into caller error")
			}
		})
	}
}

func TestCallCancellationInterruptsHandshakeAndTool(t *testing.T) {
	for _, mode := range []string{"hang", "hang-call"} {
		t.Run(mode, func(t *testing.T) {
			server := helperServer(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			start := time.Now()
			_, err := Call(ctx, t.TempDir(), server, "echo", json.RawMessage(`{"value":"hello"}`))
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cancellation result: %v", err)
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("cancellation did not promptly reap the fake process")
			}
		})
	}
	server := helperServer(t, "success")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := filepath.Join(t.TempDir(), "not-created")
	if _, err := Call(ctx, dir, server, "echo", json.RawMessage(`{}`)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pre-canceled call created state")
	}
}

func TestCallRejectsUnsafeInputsWithoutStartingProcess(t *testing.T) {
	server := helperServer(t, "success")
	for _, raw := range []string{"", "null", "[]", `"text"`, "{", `{} {}`, strings.Repeat("x", MaxMessageBytes)} {
		if _, err := Call(context.Background(), t.TempDir(), server, "echo", json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted non-object/oversized arguments %q", raw[:min(len(raw), 20)])
		}
	}
	for _, tool := range []string{"", "\n", strings.Repeat("x", 257)} {
		if _, err := Call(context.Background(), t.TempDir(), server, tool, json.RawMessage(`{}`)); err == nil {
			t.Fatal("accepted invalid tool name")
		}
	}
	for _, bad := range []workspaceconfig.MCPServer{
		{Command: "server; echo unsafe"},
		{Command: "./server"},
		{Command: server.Command, Env: map[string]string{"API_KEY": "literal-secret-key"}},
		{Command: server.Command, Env: map[string]string{"API_KEY": "UNSET_HOST_MCP_VARIABLE"}},
		{Command: server.Command, Env: map[string]string{"HOME": "HOST_MCP_HELPER_MODE"}},
	} {
		if _, err := Call(context.Background(), t.TempDir(), bad, "echo", json.RawMessage(`{}`)); err == nil {
			t.Fatal("accepted unsafe executable or secret configuration")
		}
	}
	missing := workspaceconfig.MCPServer{Command: filepath.Join(t.TempDir(), "missing-executable")}
	if _, err := Call(context.Background(), t.TempDir(), missing, "echo", json.RawMessage(`{}`)); err == nil || strings.Contains(err.Error(), missing.Command) {
		t.Fatal("start failure missing or exposed the configured path")
	}
}

func TestCallRejectsStateSymlinks(t *testing.T) {
	server := helperServer(t, "success")
	base := t.TempDir()
	outside := t.TempDir()
	linked := filepath.Join(base, "link")
	if err := os.Symlink(outside, linked); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := Call(context.Background(), linked, server, "echo", json.RawMessage(`{}`)); err == nil {
		t.Fatal("accepted symlinked state directory")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatal("state escaped into symlink target")
	}
}
