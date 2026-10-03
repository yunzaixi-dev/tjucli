// Package workspacemcp executes explicitly configured, already installed stdio
// MCP servers. It never installs packages or constructs a shell command.
package workspacemcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yunzaixi-dev/tjucli/internal/workspaceconfig"
)

const (
	ProtocolVersion = "2025-06-18"
	MaxMessageBytes = 1 << 20
	MaxOutputBytes  = 4 << 20
	MaxStderrBytes  = 64 << 10
	MaxMessages     = 128
	DefaultTimeout  = 30 * time.Second
)

var (
	ErrOutputLimit            = errors.New("MCP output limit exceeded")
	ErrSupervisionUnsupported = errors.New("MCP process-tree supervision is unsupported on this platform")
)

// Call negotiates initialize/initialized, then invokes exactly one tool using
// newline-delimited JSON-RPC. Only server.Command/Args can choose the process.
// args must be an object. No HTTP transport or caller command override exists.
// Errors intentionally omit stderr, environment values and server error text.
func Call(ctx context.Context, stateDir string, server workspaceconfig.MCPServer, tool string, args json.RawMessage) (json.RawMessage, error) {
	if err := server.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(tool) == "" || len(tool) > 256 || strings.IndexFunc(tool, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
		return nil, errors.New("invalid MCP tool name")
	}
	args = bytes.TrimSpace(args)
	if len(args) == 0 || len(args) > MaxMessageBytes/2 || args[0] != '{' || !json.Valid(args) {
		return nil, errors.New("MCP arguments must be a bounded JSON object")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Refuse before resolving secrets, creating state or executing anything.
	// Killing only the direct child is not a process-tree supervisor.
	if !processTreeSupported {
		return nil, ErrSupervisionUnsupported
	}
	dir, env, err := processEnvironment(stateDir, server)
	if err != nil {
		return nil, err
	}
	deadlineCtx, deadlineCancel := context.WithTimeout(ctx, DefaultTimeout)
	defer deadlineCancel()
	runCtx, cancel := context.WithCancelCause(deadlineCtx)
	defer cancel(nil)
	// CommandContext executes the configured binary directly. It does not use
	// sh -c, a package installer, or the user's global MCP configuration.
	cmd := exec.CommandContext(runCtx, server.Command, server.Args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.WaitDelay = 250 * time.Millisecond
	configureProcess(cmd)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, errors.New("MCP stdin setup failed")
	}
	defer in.Close()
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errors.New("MCP stdout setup failed")
	}
	defer out.Close()
	stderr := &boundedDiscard{remaining: MaxStderrBytes, cancel: cancel}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, errors.New("MCP process start failed")
	}
	// Closing pipes on cancellation interrupts scanner reads and blocked
	// writes even if a descendant inherited the server's pipes.
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-runCtx.Done():
			in.Close()
			out.Close()
		case <-stopped:
		}
	}()
	defer func() {
		in.Close()
		out.Close()
		stopProcess(cmd)
		_ = cmd.Wait()
	}()

	reader := &budgetReader{reader: out, remaining: MaxOutputBytes}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), MaxMessageBytes+1)
	session := rpcSession{in: in, scanner: scanner, ctx: runCtx}
	result, err := session.request(1, "initialize", struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ClientInfo      map[string]any `json:"clientInfo"`
	}{ProtocolVersion, map[string]any{}, map[string]any{"name": "tjuclaw", "version": "1"}})
	if err == nil {
		var initialized struct {
			ProtocolVersion string          `json:"protocolVersion"`
			Capabilities    json.RawMessage `json:"capabilities"`
			ClientInfo      json.RawMessage `json:"serverInfo"`
		}
		if json.Unmarshal(result, &initialized) != nil || !supportedVersion(initialized.ProtocolVersion) ||
			!isObject(initialized.Capabilities) || !isObject(initialized.ClientInfo) {
			err = errors.New("MCP initialization negotiation failed")
		}
	}
	if err == nil {
		err = session.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	}
	if err == nil {
		result, err = session.request(2, "tools/call", struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}{tool, args})
	}
	// Cancellation takes precedence over an EOF caused by closing pipes.
	if cause := context.Cause(runCtx); cause != nil {
		return nil, cause
	}
	if err != nil {
		return nil, err
	}
	if !isObject(result) {
		return nil, errors.New("invalid MCP tool result")
	}
	return result, nil
}

type rpcSession struct {
	in       io.Writer
	scanner  *bufio.Scanner
	ctx      context.Context
	messages int
}

func (s *rpcSession) write(message any) error {
	data, err := json.Marshal(message)
	if err != nil || len(data) > MaxMessageBytes {
		return errors.New("MCP request exceeds limit")
	}
	if _, err := s.in.Write(append(data, '\n')); err != nil {
		return errors.New("MCP request write failed")
	}
	return nil
}

func (s *rpcSession) request(id int, method string, params any) (json.RawMessage, error) {
	if err := s.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for s.scanner.Scan() {
		s.messages++
		if s.messages > MaxMessages || len(s.scanner.Bytes()) > MaxMessageBytes {
			return nil, ErrOutputLimit
		}
		if err := s.ctx.Err(); err != nil {
			return nil, err
		}
		var message struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Result  json.RawMessage `json:"result"`
			Error   json.RawMessage `json:"error"`
		}
		if json.Unmarshal(s.scanner.Bytes(), &message) != nil || message.JSONRPC != "2.0" {
			return nil, errors.New("invalid MCP JSON-RPC message")
		}
		if message.Method != "" {
			if len(message.Result) != 0 || len(message.Error) != 0 {
				return nil, errors.New("invalid MCP JSON-RPC request")
			}
			if len(message.ID) == 0 {
				// Bounded notifications (for example progress) are not replies.
				continue
			}
			if !validID(message.ID) {
				return nil, errors.New("invalid MCP server request ID")
			}
			// No roots, sampling, or elicitation capabilities are advertised.
			// Refuse server-initiated requests rather than hanging on them.
			if err := s.write(map[string]any{
				"jsonrpc": "2.0", "id": message.ID,
				"error": map[string]any{"code": -32601, "message": "Method not supported"},
			}); err != nil {
				return nil, err
			}
			continue
		}
		if string(message.ID) != fmt.Sprint(id) {
			return nil, errors.New("unexpected MCP response ID")
		}
		if len(message.Error) != 0 {
			return nil, errors.New("MCP server returned a JSON-RPC error")
		}
		if len(message.Result) == 0 {
			return nil, errors.New("MCP response missing result")
		}
		return append(json.RawMessage(nil), message.Result...), nil
	}
	if s.scanner.Err() != nil {
		if errors.Is(s.scanner.Err(), ErrOutputLimit) || errors.Is(s.scanner.Err(), bufio.ErrTooLong) {
			return nil, ErrOutputLimit
		}
		return nil, errors.New("MCP response read failed or exceeds limit")
	}
	return nil, errors.New("MCP server closed before responding")
}

func supportedVersion(version string) bool {
	switch version {
	case ProtocolVersion, "2025-03-26", "2024-11-05":
		return true
	default:
		return false
	}
}

func validID(raw json.RawMessage) bool {
	var id any
	if len(raw) > 256 || json.Unmarshal(raw, &id) != nil {
		return false
	}
	switch value := id.(type) {
	case string:
		return value != ""
	case float64:
		return value == float64(int64(value))
	default:
		return false
	}
}

func isObject(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) != 0 && raw[0] == '{' && json.Valid(raw)
}

type budgetReader struct {
	reader    io.Reader
	remaining int
}

func (r *budgetReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, ErrOutputLimit
	}
	if len(p) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	r.remaining -= n
	return n, err
}

type boundedDiscard struct {
	mu        sync.Mutex
	remaining int
	cancel    context.CancelCauseFunc
}

func (w *boundedDiscard) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(p) > w.remaining {
		w.cancel(ErrOutputLimit)
		return 0, ErrOutputLimit
	}
	w.remaining -= len(p)
	return len(p), nil
}

func processEnvironment(stateDir string, server workspaceconfig.MCPServer) (string, []string, error) {
	// Include only execution essentials, not arbitrary ambient credentials.
	values := map[string]string{}
	for _, key := range []string{"PATH", "SystemRoot", "WINDIR", "COMSPEC", "PATHEXT"} {
		if value, ok := os.LookupEnv(key); ok {
			values[key] = value
		}
	}
	reserved := map[string]string{
		"HOME": "home", "USERPROFILE": "home",
		"XDG_CONFIG_HOME": "config", "APPDATA": "config",
		"XDG_CACHE_HOME": "cache", "LOCALAPPDATA": "cache",
		"XDG_DATA_HOME": "data", "XDG_STATE_HOME": "state",
		"TMPDIR": "tmp", "TMP": "tmp", "TEMP": "tmp",
	}
	for child, host := range server.Env {
		if _, ok := reserved[strings.ToUpper(child)]; ok {
			return "", nil, errors.New("MCP cannot override independent state directories")
		}
		value, ok := os.LookupEnv(host)
		if !ok {
			return "", nil, errors.New("MCP referenced environment variable is unset")
		}
		values[child] = value
	}
	root, err := workspaceconfig.OpenPrivateDir(stateDir)
	if err != nil {
		return "", nil, err
	}
	root.Close()
	identity, _ := json.Marshal(server)
	hash := sha256.Sum256(identity)
	dir := filepath.Join(stateDir, "mcp-"+hex.EncodeToString(hash[:16]))
	root, err = workspaceconfig.OpenPrivateDir(dir)
	if err != nil {
		return "", nil, err
	}
	root.Close()
	for key, child := range reserved {
		path := filepath.Join(dir, child)
		if values[key] == path {
			continue
		}
		root, err = workspaceconfig.OpenPrivateDir(path)
		if err != nil {
			return "", nil, err
		}
		root.Close()
		values[key] = path
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	env := make([]string, 0, len(values))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return dir, env, nil
}
