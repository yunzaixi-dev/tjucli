package workspaceruntime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yunzaixi-dev/tjucli/internal/workspaceconfig"
)

// Codex uses newline-delimited JSON-RPC-like messages (without jsonrpc).
// Wire enums below were checked against codex-cli 0.160.0's generated schema;
// some official prose examples use older enum spellings.
type codexMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

type codexConnection struct {
	scanner     *bufio.Scanner
	writer      *json.Encoder
	denied      bool
	ctx         context.Context
	executor    Executor
	state       runState
	activeTurn  string
	approvalIDs map[string]bool
}

func (e Executor) codex(ctx context.Context, state runState, endpoint workspaceconfig.Endpoint, prompt string) (text string, err error) {
	home := filepath.Join(state.session.dir, "codex")
	keyConfig := "env_key = \"" + keyVariable + "\"\n"
	if endpoint.APIKeyEnv == "" {
		keyConfig = "" // No authentication header required by this local provider.
	}
	// Key names, never key values, are persisted. No global account/config
	// fallback: use only this invocation's Responses-compatible provider.
	config := "model_provider = \"workspace\"\n" +
		"model = " + strconv.Quote(endpoint.Model) + "\n" +
		"approval_policy = \"untrusted\"\nsandbox_mode = \"read-only\"\n" +
		"[features]\nhooks = false\n" +
		"[model_providers.workspace]\nname = \"Workspace\"\n" +
		"base_url = " + strconv.Quote(endpoint.BaseURL) + "\n" +
		keyConfig + "wire_api = \"responses\"\nrequires_openai_auth = false\n"
	if state.auth.Mode != "" {
		config = "model_provider = \"openai\"\ncli_auth_credentials_store = \"file\"\n" +
			"approval_policy = \"untrusted\"\nsandbox_mode = \"read-only\"\n" +
			"[features]\nhooks = false\n"
		if endpoint.Model != "" {
			config = "model = " + strconv.Quote(endpoint.Model) + "\n" + config
		}
	}
	if privateBytes(filepath.Join(home, "config.toml"), []byte(config)) != nil {
		return "", ErrInvalidConfig
	}
	state.env = append(state.env, "CODEX_HOME="+home)
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	args := []string{"app-server", "--listen", "stdio://",
		"-c", "features.hooks=false", "-c", `approval_policy="untrusted"`, "-c", `sandbox_mode="read-only"`,
		"-c", `cli_auth_credentials_store="file"`}
	if state.auth.Mode != "" {
		args = append(args, "-c", `model_provider="openai"`)
	}
	cmd, err := e.command(childCtx, state, "codex", args)
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
	// Interrupt blocked RPC writes/reads even if a descendant retained a
	// pipe or the platform can only kill the immediate process.
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
		_ = cmd.Wait() // a completed turn is authoritative; the server is long-lived
		if stderr.overflow() {
			text, err = "", ErrOutputLimit
		} else if ctx.Err() != nil {
			text, err = "", ErrCanceled
		}
	}()
	scanner := bufio.NewScanner(&budgetReader{reader: stdout, left: maxOutput, cancel: cancel})
	scanner.Buffer(make([]byte, 4096), maxOutput)
	rpc := codexConnection{scanner: scanner, writer: json.NewEncoder(stdin), ctx: ctx, executor: e, state: state}
	return rpc.prompt(e.Config.Root, endpoint.Model, prompt)
}

func (c *codexConnection) send(value any) error {
	if c.writer.Encode(value) != nil {
		return ErrRuntimeFailed
	}
	return nil
}

func (c *codexConnection) read() (codexMessage, error) {
	var message codexMessage
	if !c.scanner.Scan() {
		if errors.Is(c.scanner.Err(), ErrOutputLimit) || errors.Is(c.scanner.Err(), bufio.ErrTooLong) {
			return message, ErrOutputLimit
		}
		return message, ErrRuntimeFailed
	}
	if errors.Is(c.scanner.Err(), ErrOutputLimit) {
		return message, ErrOutputLimit
	}
	if json.Unmarshal(c.scanner.Bytes(), &message) != nil {
		return message, ErrRuntimeFailed
	}
	return message, nil
}

func (c *codexConnection) reject(message codexMessage) error {
	var result any
	switch message.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		decision := "decline"
		var scope struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
		}
		if c.approvalIDs == nil {
			c.approvalIDs = map[string]bool{}
		}
		id := string(message.ID)
		validID := len(id) <= 256 && id != "" && id != "null" && !c.approvalIDs[id] && len(c.approvalIDs) < 256
		c.approvalIDs[id] = true
		// Do not route an approval for an unrelated native thread.
		if json.Unmarshal(message.Params, &scope) == nil &&
			c.state.session != nil && scope.ThreadID == c.state.session.NativeID &&
			c.activeTurn != "" && scope.TurnID == c.activeTurn && validID &&
			c.executor.approve(c.ctx, c.state, "codex", message.Method, "", message.Params) {
			decision = "accept" // one request only, never acceptForSession/amendments
		} else {
			c.denied = true
		}
		result = map[string]any{"decision": decision}
	case "item/permissions/requestApproval":
		c.denied = true
		result = map[string]any{"permissions": map[string]any{}, "scope": "turn"}
	case "mcpServer/elicitation/request":
		c.denied = true
		result = map[string]any{"action": "decline", "content": nil}
	default:
		c.denied = true
		// Unknown tool/user-input/auth requests are never accepted or echoed.
		return c.send(map[string]any{"id": message.ID,
			"error": map[string]any{"code": -32601, "message": "unsupported host request"}})
	}
	return c.send(map[string]any{"id": message.ID, "result": result})
}

func (c *codexConnection) call(id int, method string, params any, target any) error {
	if c.send(map[string]any{"id": id, "method": method, "params": params}) != nil {
		return ErrRuntimeFailed
	}
	for {
		message, err := c.read()
		if err != nil {
			return err
		}
		if message.Method != "" && len(message.ID) != 0 {
			if c.reject(message) != nil {
				return ErrRuntimeFailed
			}
			continue
		}
		if message.Method != "" {
			// Lifecycle notifications can precede the response. Completion
			// before a turn/start response is not the expected protocol.
			if message.Method == "turn/completed" {
				return ErrRuntimeFailed
			}
			continue
		}
		if !bytes.Equal(message.ID, []byte(strconv.Itoa(id))) ||
			(len(message.Error) != 0 && string(message.Error) != "null") ||
			len(message.Result) == 0 || json.Unmarshal(message.Result, target) != nil {
			return ErrRuntimeFailed
		}
		return nil
	}
}

func (c *codexConnection) prompt(root, model, prompt string) (string, error) {
	var initialized map[string]any
	if err := c.call(1, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "tjuclaw", "title": "TJUClaw", "version": "1"},
	}, &initialized); err != nil {
		return "", err
	}
	if c.send(map[string]any{"method": "initialized", "params": map[string]any{}}) != nil {
		return "", ErrRuntimeFailed
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	params := map[string]any{
		"cwd": root, "model": model, "modelProvider": "workspace",
		"approvalPolicy": "untrusted", "sandbox": "read-only", "ephemeral": false,
	}
	method := "thread/start"
	if c.state.auth.Mode != "" {
		params["modelProvider"] = "openai"
		if model == "" {
			delete(params, "model")
		}
	}
	params["approvalsReviewer"] = "user"
	if c.state.session.NativeID != "" {
		method = "thread/resume"
		delete(params, "ephemeral")
		params["threadId"] = c.state.session.NativeID
	}
	if err := c.call(2, method, params, &started); err != nil {
		return "", err
	}
	if started.Thread.ID == "" || len(started.Thread.ID) > 256 {
		return "", ErrRuntimeFailed
	}
	if method == "thread/resume" && started.Thread.ID != c.state.session.NativeID {
		return "", ErrSessionUnavailable
	}
	c.state.session.NativeID = started.Thread.ID
	if c.state.session.save() != nil {
		return "", ErrInvalidConfig
	}
	var begun struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	turnParams := map[string]any{
		"threadId": started.Thread.ID, "cwd": root, "model": model,
		"approvalPolicy": "untrusted",
		"sandboxPolicy":  map[string]any{"type": "readOnly", "networkAccess": false},
		"input":          []any{map[string]string{"type": "text", "text": prompt}},
	}
	if model == "" {
		delete(turnParams, "model")
	}
	if err := c.call(3, "turn/start", turnParams, &begun); err != nil {
		return "", err
	}
	if begun.Turn.ID == "" || len(begun.Turn.ID) > 256 {
		return "", ErrRuntimeFailed
	}
	c.activeTurn = begun.Turn.ID
	var output strings.Builder
	found := false
	seen := map[string]bool{}
	for {
		message, err := c.read()
		if err != nil {
			return "", err
		}
		if message.Method != "" && len(message.ID) != 0 {
			if c.reject(message) != nil {
				return "", ErrRuntimeFailed
			}
			continue
		}
		switch message.Method {
		case "item/completed":
			var event struct {
				ThreadID string `json:"threadId"`
				TurnID   string `json:"turnId"`
				Item     struct {
					ID   string `json:"id"`
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"item"`
			}
			if json.Unmarshal(message.Params, &event) != nil ||
				event.ThreadID != started.Thread.ID || event.TurnID != begun.Turn.ID {
				return "", ErrRuntimeFailed
			}
			if event.Item.Type == "agentMessage" {
				if event.Item.ID == "" || seen[event.Item.ID] || strings.ContainsRune(event.Item.Text, 0) {
					return "", ErrRuntimeFailed
				}
				seen[event.Item.ID], found = true, true
				if output.Len() != 0 {
					output.WriteByte('\n')
				}
				output.WriteString(event.Item.Text)
			}
		case "turn/completed":
			var event struct {
				ThreadID string `json:"threadId"`
				Turn     struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"turn"`
			}
			if json.Unmarshal(message.Params, &event) != nil || event.ThreadID != started.Thread.ID ||
				event.Turn.ID != begun.Turn.ID {
				return "", ErrRuntimeFailed
			}
			if c.denied {
				return "", ErrApprovalRequired
			}
			if event.Turn.Status != "completed" || !found {
				return "", ErrRuntimeFailed
			}
			return output.String(), nil
		}
	}
}
