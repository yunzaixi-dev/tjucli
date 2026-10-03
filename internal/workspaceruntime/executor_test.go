package workspaceruntime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yunzaixi-dev/tjucli/internal/workspaceconfig"
)

// All runtime executables in these tests are links to this test binary.
// No installed agent, developer account, model endpoint, or paid prompt runs.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "--fake-mcp" {
		fakeMCP()
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "--config-dir" {
		fakeBridgeCLI()
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "--fake-descendant" {
		_ = os.WriteFile(os.Args[2], []byte(fmt.Sprint(os.Getpid())), 0600)
		time.Sleep(10 * time.Minute)
		os.Exit(0)
	}
	switch filepath.Base(os.Args[0]) {
	case "pi", "claude", "codex":
		fakeRuntime(filepath.Base(os.Args[0]))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type processRecord struct {
	Args  []string          `json:"args"`
	Env   []string          `json:"env"`
	CWD   string            `json:"cwd"`
	Input string            `json:"input"`
	Files map[string]string `json:"files"`
	RPC   []json.RawMessage `json:"rpc"`
}

func fakeRuntime(name string) {
	cwd, _ := os.Getwd()
	mode, _ := os.ReadFile(filepath.Join(cwd, ".fake-mode"))
	record := processRecord{Args: os.Args[1:], Env: os.Environ(), CWD: cwd, Files: map[string]string{}}
	for _, file := range []string{
		filepath.Join(os.Getenv("PI_CODING_AGENT_DIR"), "models.json"),
		filepath.Join(os.Getenv("PI_CODING_AGENT_DIR"), "settings.json"),
		filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"),
		filepath.Join(os.Getenv("PI_CODING_AGENT_DIR"), "workspace-bridge.ts"),
		filepath.Join(os.Getenv("CODEX_HOME"), "auth.json"),
		filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".credentials.json"),
	} {
		if data, err := os.ReadFile(file); err == nil {
			record.Files[filepath.Base(file)] = string(data)
		}
	}
	save := func() {
		data, _ := json.Marshal(record)
		_ = os.WriteFile(filepath.Join(cwd, ".fake-record"), data, 0600)
	}
	save()
	switch string(mode) {
	case "hang":
		time.Sleep(10 * time.Minute)
	case "descendant":
		self, _ := os.Executable()
		child := exec.Command(self, "--fake-descendant", filepath.Join(cwd, ".fake-descendant-pid"))
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if child.Start() != nil {
			os.Exit(3)
		}
		time.Sleep(10 * time.Minute)
	case "stdout-flood":
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), maxOutput+8192))
		time.Sleep(10 * time.Minute)
	case "stderr-flood":
		_, _ = os.Stderr.Write(bytes.Repeat([]byte("x"), maxStderr+8192))
		time.Sleep(10 * time.Minute)
	case "failure":
		fmt.Fprintln(os.Stderr, "private diagnostics fake-secret https://gateway.example.test/private/v1")
		os.Exit(2)
	case "malformed":
		fmt.Println("not json")
		return
	}
	text := "ok"
	if string(mode) == "echo-secret" {
		text = os.Getenv(keyVariable) + " https://gateway.example.test/private/v1"
	}
	if string(mode) == "echo-mcp-secret" {
		text = "leaked " + os.Getenv("BRIDGE_MCP_HOST")
		// The adapter must redact the original host value, even though the
		// fake Pi (like real Pi) does not inherit it in its environment.
		data, _ := os.ReadFile(filepath.Join(os.Getenv("PI_CODING_AGENT_DIR"), "mcp-env.json"))
		var values map[string]map[string]string
		_ = json.Unmarshal(data, &values)
		text += values["local.demo"]["BRIDGE_MCP_HOST"]
	}
	if string(mode) == "echo-host-secret" {
		text = "host-native-secret"
	}
	if name == "codex" {
		fakeCodex(&record, save, string(mode), text)
		return
	}
	if name == "claude" && slices.Contains(record.Args, "stream-json") {
		fakeClaudeStream(&record, save, string(mode), text)
		return
	}
	input, _ := io.ReadAll(io.LimitReader(os.Stdin, maxArguments+1))
	record.Input = string(input)
	if name == "pi" || name == "claude" {
		turn := fakeNativeHistory(name, record.Args, string(input))
		if string(mode) == "session-history" {
			text = fmt.Sprintf("turn %d", turn)
		}
	}
	save()
	if name == "pi" {
		stop := "stop"
		if string(mode) == "agent-error" {
			stop = "error"
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "message_end", "message": map[string]any{
			"role": "assistant", "stopReason": stop, "content": []any{map[string]string{"type": "text", "text": text}},
		}})
		if string(mode) != "missing-completion" {
			fmt.Println(`{"type":"agent_end","messages":[]}`)
		}
		return
	}
	result := map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": text}
	if string(mode) == "agent-error" {
		result["is_error"] = true
	}
	if string(mode) == "approval" {
		result["permission_denials"] = []any{map[string]string{"tool_name": "Bash", "tool_use_id": "denied"}}
	}
	_ = json.NewEncoder(os.Stdout).Encode(result)
}

func fakeCodex(record *processRecord, save func(), mode, text string) {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), maxArguments+4096)
	out := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		record.RPC = append(record.RPC, append(json.RawMessage(nil), scanner.Bytes()...))
		save()
		var request codexMessage
		_ = json.Unmarshal(scanner.Bytes(), &request)
		switch request.Method {
		case "initialize":
			if mode == "rpc-error" {
				_ = out.Encode(map[string]any{"id": request.ID, "error": map[string]any{"message": "private fake-secret"}})
				return
			}
			_ = out.Encode(map[string]any{"id": request.ID, "result": map[string]any{"userAgent": "fake"}})
		case "thread/start", "thread/resume":
			path := filepath.Join(os.Getenv("CODEX_HOME"), "fake-thread.json")
			if request.Method == "thread/start" {
				_ = os.WriteFile(path, []byte(`{"id":"thread"}`), 0600)
			} else if _, err := os.Stat(path); err != nil {
				_ = out.Encode(map[string]any{"id": request.ID, "error": map[string]string{"message": "missing native thread"}})
				return
			}
			_ = out.Encode(map[string]any{"method": "thread/started", "params": map[string]any{}})
			_ = out.Encode(map[string]any{"id": request.ID, "result": map[string]any{"thread": map[string]string{"id": "thread"}}})
			if mode == "stdin-block" {
				time.Sleep(10 * time.Minute)
			}
		case "turn/start":
			record.Input = string(request.Params)
			save()
			var turn struct {
				Input []struct{ Text string } `json:"input"`
			}
			_ = json.Unmarshal(request.Params, &turn)
			if len(turn.Input) != 0 {
				n := fakeAppendHistory(filepath.Join(os.Getenv("CODEX_HOME"), "fake-history.jsonl"), turn.Input[0].Text)
				if mode == "session-history" {
					text = fmt.Sprintf("turn %d", n)
				}
			}
			_ = out.Encode(map[string]any{"id": request.ID, "result": map[string]any{"turn": map[string]string{"id": "turn"}}})
			if mode == "approval" || mode == "permission-approval" || mode == "unknown-request" {
				method := "item/commandExecution/requestApproval"
				if mode == "permission-approval" {
					method = "item/permissions/requestApproval"
				} else if mode == "unknown-request" {
					method = "unknown/tool"
				}
				_ = out.Encode(map[string]any{"id": "approval", "method": method,
					"params": map[string]any{"threadId": "thread", "turnId": "turn", "itemId": "item", "command": "echo fake-secret"}})
				continue
			}
			if mode == "notification-flood" {
				for i := 0; i < maxOutput/100; i++ {
					_ = out.Encode(map[string]any{"method": "item/agentMessage/delta",
						"params": map[string]string{"delta": strings.Repeat("x", 100)}})
				}
				continue
			}
			fakeCodexDone(out, mode, text)
		default:
			if string(request.ID) == `"approval"` {
				fakeCodexDone(out, mode, text)
			}
		}
	}
}

func fakeCodexDone(out *json.Encoder, mode, text string) {
	thread := "thread"
	if mode == "wrong-thread" {
		thread = "another-thread"
	}
	_ = out.Encode(map[string]any{"method": "item/completed", "params": map[string]any{
		"threadId": thread, "turnId": "turn", "item": map[string]string{"id": "item", "type": "agentMessage", "text": text},
	}})
	status := "completed"
	if mode == "agent-error" {
		status = "failed"
	}
	_ = out.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{
		"threadId": thread, "turn": map[string]string{"id": "turn", "status": status},
	}})
}

func fakeMCP() {
	recordFile := filepath.Join(os.Args[2], ".fake-mcp-record")
	environment, _ := json.Marshal(os.Environ())
	_ = os.WriteFile(filepath.Join(os.Args[2], ".fake-mcp-env"), environment, 0600)
	var messages []json.RawMessage
	scanner := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		messages = append(messages, append(json.RawMessage(nil), scanner.Bytes()...))
		data, _ := json.Marshal(messages)
		_ = os.WriteFile(recordFile, data, 0600)
		var request codexMessage
		_ = json.Unmarshal(scanner.Bytes(), &request)
		switch request.Method {
		case "initialize":
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{
				"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "serverInfo": map[string]string{"name": "fake", "version": "1"},
			}})
		case "tools/call":
			var args struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(request.Params, &args)
			value := "mcp ok"
			if args.Name == "echo-secret" {
				value = os.Getenv("MCP_KEY")
			}
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{
				"content": []any{map[string]string{"type": "text", "text": value}},
			}})
		}
	}
}

func fakeExecutor(t *testing.T, capabilities ...string) Executor {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake executable links require Unix; native Windows process-tree support is not claimed")
	}
	root, state, bin := t.TempDir(), t.TempDir(), t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, binary := range []string{"pi", "claude", "codex"} {
		if err := os.Symlink(self, filepath.Join(bin, binary)); err != nil {
			t.Fatal(err)
		}
	}
	// A nonexistent runtime cannot accidentally fall back to installed agents.
	t.Setenv("PATH", bin)
	t.Setenv("WORKSPACE_TEST_KEY", "fake-secret")
	t.Setenv("OPENAI_API_KEY", "unrelated-developer-secret")
	t.Setenv("ANTHROPIC_API_KEY", "unrelated-developer-secret")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "unrelated-developer-secret")
	t.Setenv("NODE_OPTIONS", "--unwanted-option")
	t.Setenv("PI_CODING_AGENT_DIR", "/unwanted/global/pi")
	t.Setenv("CODEX_HOME", "/unwanted/global/codex")
	return Executor{Config: workspaceconfig.Config{
		Version: 1, ID: "independent-identity", Name: "test", Root: root,
		AllowedCapabilities: capabilities,
		Endpoints:           []workspaceconfig.Endpoint{{Name: "local", Model: "model-test", BaseURL: "https://gateway.example.test/private/v1", APIKeyEnv: "WORKSPACE_TEST_KEY"}},
	}, StateDir: state}
}

func setMode(t *testing.T, e Executor, mode string) {
	t.Helper()
	if os.WriteFile(filepath.Join(e.Config.Root, ".fake-mode"), []byte(mode), 0600) != nil {
		t.Fatal("set fake mode")
	}
}

func readRecord(t *testing.T, e Executor) processRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(e.Config.Root, ".fake-record"))
	if err != nil {
		t.Fatal(err)
	}
	var record processRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func envValue(env []string, name string) string {
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, name+"="); ok {
			return value
		}
	}
	return ""
}

func TestPermissionCheckComesFirst(t *testing.T) {
	e := Executor{}
	for _, capability := range []string{"pi.prompt", "claude.prompt", "codex.prompt", "mcp.call", "shell"} {
		_, err := e.Execute(context.Background(), capability, json.RawMessage(`not json`))
		if !errors.Is(err, ErrCapabilityDenied) {
			t.Fatalf("%s: %v", capability, err)
		}
	}
	e.Config.AllowedCapabilities = []string{"shell"}
	if _, err := e.Execute(context.Background(), "shell", nil); !errors.Is(err, ErrCapabilityDenied) {
		t.Fatal(err)
	}
}

func TestStrictPromptArguments(t *testing.T) {
	e := fakeExecutor(t, "pi.prompt")
	for _, raw := range []string{
		`null`, `[]`, `{}`, `{"prompt":null}`, `{"prompt":1}`, `{"prompt":""}`,
		`{"prompt":"ok","cwd":"/tmp"}`, `{"prompt":"ok","args":["--approve"]}`,
		`{"prompt":"ok","command":"sh"}`, `{"prompt":"ok","api_key":"secret"}`,
		`{"prompt":"ok","endpoint":null}`, `{"prompt":"ok","endpoint":42}`,
		`{"prompt":"first","prompt":"second"}`, `{"prompt":"ok"} {}`,
		`{"prompt":"\u0000"}`, `{"prompt":"` + strings.Repeat("x", maxPrompt+1) + `"}`,
	} {
		if _, err := e.Execute(context.Background(), "pi.prompt", json.RawMessage(raw)); !errors.Is(err, ErrInvalidArguments) {
			t.Errorf("%.80s: %v", raw, err)
		}
	}
	if _, err := os.Stat(filepath.Join(e.StateDir, "runtime")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid arguments touched runtime state")
	}
}

func TestNoGlobalAccountOrEndpointFallback(t *testing.T) {
	e := fakeExecutor(t, "pi.prompt")
	raw := json.RawMessage(`{"prompt":"ok"}`)
	e.Config.Endpoints = nil
	if _, err := e.Execute(context.Background(), "pi.prompt", raw); !errors.Is(err, ErrEndpointRequired) {
		t.Fatal(err)
	}
	e.Config.Endpoints = []workspaceconfig.Endpoint{{Name: "local", BaseURL: "https://example.test/v1", Model: "test", APIKeyEnv: "UNSET_RUNTIME_TEST_KEY"}}
	t.Setenv("UNSET_RUNTIME_TEST_KEY", "")
	if _, err := e.Execute(context.Background(), "pi.prompt", raw); !errors.Is(err, ErrEndpointUnavailable) {
		t.Fatal(err)
	}
	if _, err := e.Execute(context.Background(), "pi.prompt", json.RawMessage(`{"prompt":"ok","endpoint":"https://attacker.test"}`)); !errors.Is(err, ErrEndpointUnavailable) {
		t.Fatal(err)
	}
}

func TestPromptProcessesHaveFixedCWDAndIsolatedEnvironment(t *testing.T) {
	for _, capability := range []string{"pi.prompt", "claude.prompt", "codex.prompt"} {
		t.Run(capability, func(t *testing.T) {
			e := fakeExecutor(t, capability)
			prompt := "--dangerously-bypass-approvals-and-sandbox @/etc/passwd; $(touch injected)"
			raw, _ := json.Marshal(promptArgs{Prompt: prompt})
			result, err := e.Execute(context.Background(), capability, raw)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(result, []byte(`"output":"ok"`)) {
				t.Fatal(string(result))
			}
			record := readRecord(t, e)
			if record.CWD != e.Config.Root {
				t.Fatal("cwd is not Config.Root")
			}
			if slices.Contains(record.Args, prompt) {
				t.Fatal("prompt passed in argv")
			}
			for _, entry := range record.Env {
				if strings.Contains(entry, "unrelated-developer-secret") || strings.Contains(entry, "/unwanted/global") ||
					strings.HasPrefix(entry, "NODE_OPTIONS=") {
					t.Fatalf("inherited ambient state: %s", entry)
				}
			}
			if envValue(record.Env, keyVariable) != "fake-secret" {
				t.Fatal("selected key missing")
			}
			home := envValue(record.Env, "HOME")
			if !strings.HasPrefix(home, filepath.Join(e.StateDir, "runtime")+string(filepath.Separator)) {
				t.Fatal("home escaped state")
			}
			if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("per-invocation credential state not cleaned")
			}
			for _, contents := range record.Files {
				if strings.Contains(contents, "fake-secret") {
					t.Fatal("key persisted in generated config")
				}
			}
			switch capability {
			case "pi.prompt":
				if record.Input != prompt || slices.Contains(record.Args, "--no-tools") || !slices.Contains(record.Args, "--no-approve") {
					t.Fatal("Pi prompt/permission flags")
				}
				var models map[string]any
				if json.Unmarshal([]byte(record.Files["models.json"]), &models) != nil ||
					!strings.Contains(record.Files["models.json"], `"$`+keyVariable+`"`) {
					t.Fatal("Pi custom endpoint config")
				}
			case "claude.prompt":
				if record.Input != prompt || !slices.Contains(record.Args, "--permission-prompts") ||
					!slices.Contains(record.Args, "none") || !slices.Contains(record.Args, "--bare") {
					t.Fatal("Claude safe print flags")
				}
				if envValue(record.Env, "ANTHROPIC_API_KEY") != "fake-secret" {
					t.Fatal("Claude selected key")
				}
			case "codex.prompt":
				if len(record.RPC) != 4 || !slices.Contains(record.Args, "app-server") {
					t.Fatalf("Codex handshake: %d", len(record.RPC))
				}
				if !bytes.Contains(record.RPC[2], []byte(`"sandbox":"read-only"`)) ||
					!bytes.Contains(record.RPC[2], []byte(`"approvalPolicy":"untrusted"`)) {
					t.Fatal("Codex unsafe thread policy")
				}
				var turn struct {
					Params struct {
						Input []struct{ Text string } `json:"input"`
					} `json:"params"`
				}
				_ = json.Unmarshal(record.RPC[3], &turn)
				if len(turn.Params.Input) != 1 || turn.Params.Input[0].Text != prompt {
					t.Fatal("Codex prompt is not literal text")
				}
			}
		})
	}
}

func TestModelKeyAndEndpointAreNeverReturned(t *testing.T) {
	for _, capability := range []string{"pi.prompt", "claude.prompt", "codex.prompt"} {
		t.Run(capability, func(t *testing.T) {
			e := fakeExecutor(t, capability)
			setMode(t, e, "echo-secret")
			result, err := e.Execute(context.Background(), capability, json.RawMessage(`{"prompt":"ok"}`))
			if err != nil || bytes.Contains(result, []byte("fake-secret")) || bytes.Contains(result, []byte("gateway.example.test")) {
				t.Fatalf("redaction failed: error=%v", err)
			}
		})
	}
}

func TestProcessFailuresAreBoundedAndPublicSafe(t *testing.T) {
	for _, capability := range []string{"pi.prompt", "claude.prompt", "codex.prompt"} {
		for _, mode := range []string{"failure", "malformed", "agent-error", "stdout-flood", "stderr-flood"} {
			t.Run(capability+"/"+mode, func(t *testing.T) {
				e := fakeExecutor(t, capability)
				setMode(t, e, mode)
				want := ErrRuntimeFailed
				if strings.HasSuffix(mode, "flood") {
					want = ErrOutputLimit
				}
				_, err := e.Execute(context.Background(), capability, json.RawMessage(`{"prompt":"ok"}`))
				if !errors.Is(err, want) {
					t.Fatalf("want %v, got %v", want, err)
				}
				if strings.Contains(err.Error(), "fake-secret") || strings.Contains(err.Error(), "gateway.example.test") {
					t.Fatal("unsafe error")
				}
			})
		}
	}
}

func TestCancellationKillsPromptProcesses(t *testing.T) {
	for _, capability := range []string{"pi.prompt", "claude.prompt", "codex.prompt"} {
		t.Run(capability, func(t *testing.T) {
			e := fakeExecutor(t, capability)
			setMode(t, e, "hang")
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			start := time.Now()
			_, err := e.Execute(ctx, capability, json.RawMessage(`{"prompt":"ok"}`))
			if !errors.Is(err, ErrCanceled) || time.Since(start) > 3*time.Second {
				t.Fatalf("cancellation: %v after %s", err, time.Since(start))
			}
		})
	}
}

func TestRuntimeUnavailableDoesNotFallBack(t *testing.T) {
	e := fakeExecutor(t, "pi.prompt")
	if os.Remove(filepath.Join(os.Getenv("PATH"), "pi")) != nil {
		t.Fatal("remove fake binary")
	}
	_, err := e.Execute(context.Background(), "pi.prompt", json.RawMessage(`{"prompt":"ok"}`))
	if !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatal(err)
	}
}

func TestCodexCancellationInterruptsBlockedRPCWrite(t *testing.T) {
	e := fakeExecutor(t, "codex.prompt")
	setMode(t, e, "stdin-block")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	raw, _ := json.Marshal(promptArgs{Prompt: strings.Repeat("x", maxPrompt)})
	start := time.Now()
	_, err := e.Execute(ctx, "codex.prompt", raw)
	if !errors.Is(err, ErrCanceled) || time.Since(start) > 3*time.Second {
		t.Fatalf("blocked write cancellation: %v after %s", err, time.Since(start))
	}
}

func TestExplicitEndpointAndPluginSelection(t *testing.T) {
	e := fakeExecutor(t, "pi.prompt")
	e.Config.Endpoints = append(e.Config.Endpoints, workspaceconfig.Endpoint{
		Name: "other", Model: "other-model", BaseURL: "https://other.example.test/v1", APIKeyEnv: "OTHER_TEST_KEY",
	})
	t.Setenv("OTHER_TEST_KEY", "other-fake-key")
	plugin := filepath.Join(t.TempDir(), "approved-plugin.ts")
	e.Config.Plugins = []string{plugin}
	raw := json.RawMessage(`{"prompt":"ok","endpoint":"other"}`)
	if _, err := e.Execute(context.Background(), "pi.prompt", raw); err != nil {
		t.Fatal(err)
	}
	record := readRecord(t, e)
	if envValue(record.Env, keyVariable) != "other-fake-key" || !slices.Contains(record.Args, plugin) ||
		!slices.Contains(record.Args, "--no-extensions") ||
		!strings.Contains(record.Files["models.json"], "other.example.test") ||
		strings.Contains(record.Files["models.json"], "gateway.example.test") {
		t.Fatal("endpoint/plugin isolation")
	}
	if _, err := e.Execute(context.Background(), "pi.prompt", json.RawMessage(`{"prompt":"ok"}`)); !errors.Is(err, ErrEndpointRequired) {
		t.Fatal("ambiguous endpoint was selected implicitly", err)
	}
	e.Config.Plugins = []string{"npm:unreviewed-package"}
	if _, err := e.Execute(context.Background(), "pi.prompt", raw); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal("adapter accepted auto-installable plugin", err)
	}
}

func TestRedirectedRuntimeStateIsRejected(t *testing.T) {
	e := fakeExecutor(t, "pi.prompt")
	path := filepath.Join(e.StateDir, "runtime")
	if os.Symlink(t.TempDir(), path) != nil {
		t.Fatal("make redirected runtime state")
	}
	if _, err := e.Execute(context.Background(), "pi.prompt", json.RawMessage(`{"prompt":"ok"}`)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal("runtime state symlink accepted", err)
	}
}

func TestCodexApprovalRequestsAreDenied(t *testing.T) {
	for _, mode := range []string{"approval", "permission-approval", "unknown-request"} {
		t.Run(mode, func(t *testing.T) {
			e := fakeExecutor(t, "codex.prompt")
			setMode(t, e, mode)
			_, err := e.Execute(context.Background(), "codex.prompt", json.RawMessage(`{"prompt":"ok"}`))
			if !errors.Is(err, ErrApprovalRequired) {
				t.Fatal(err)
			}
			record := readRecord(t, e)
			last := record.RPC[len(record.RPC)-1]
			if bytes.Contains(last, []byte("accept")) || (!bytes.Contains(last, []byte("decline")) &&
				!bytes.Contains(last, []byte(`"permissions":{}`)) && !bytes.Contains(last, []byte(`"error"`))) {
				t.Fatal("approval request was not safely rejected")
			}
		})
	}
}

func TestCodexRejectsWrongThreadAndProtocolErrors(t *testing.T) {
	for _, mode := range []string{"wrong-thread", "rpc-error", "notification-flood"} {
		t.Run(mode, func(t *testing.T) {
			e := fakeExecutor(t, "codex.prompt")
			setMode(t, e, mode)
			_, err := e.Execute(context.Background(), "codex.prompt", json.RawMessage(`{"prompt":"ok"}`))
			want := ErrRuntimeFailed
			if mode == "notification-flood" {
				want = ErrOutputLimit
			}
			if !errors.Is(err, want) {
				t.Fatal(err)
			}
		})
	}
}

func TestClaudePermissionDenialsAndPiIncompleteTurn(t *testing.T) {
	for _, tc := range []struct {
		capability, mode string
		want             error
	}{{"claude.prompt", "approval", ErrApprovalRequired}, {"pi.prompt", "missing-completion", ErrRuntimeFailed}} {
		t.Run(tc.capability, func(t *testing.T) {
			e := fakeExecutor(t, tc.capability)
			setMode(t, e, tc.mode)
			if _, err := e.Execute(context.Background(), tc.capability, json.RawMessage(`{"prompt":"ok"}`)); !errors.Is(err, tc.want) {
				t.Fatal(err)
			}
		})
	}
}

func TestWorkspaceStateIsSeparateEvenWithSharedCWD(t *testing.T) {
	e := fakeExecutor(t, "pi.prompt")
	second := e
	second.StateDir = t.TempDir()
	for _, executor := range []Executor{e, second} {
		if _, err := executor.Execute(context.Background(), "pi.prompt", json.RawMessage(`{"prompt":"ok"}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(executor.StateDir, "runtime", "pi", "sessions")); err != nil {
			t.Fatal(err)
		}
		record := readRecord(t, executor)
		if envValue(record.Env, "PI_CODING_AGENT_SESSION_DIR") != filepath.Join(executor.StateDir, "runtime", "pi", "sessions") {
			t.Fatal("sessions keyed by cwd instead of environment")
		}
	}
}

func TestMCPForwardsOnlyConfiguredServerAfterPermissionCheck(t *testing.T) {
	e := fakeExecutor(t, "mcp.call")
	self, _ := os.Executable()
	e.Config.MCPServers = map[string]workspaceconfig.MCPServer{"local": {
		Command: self, Args: []string{"--fake-mcp", e.Config.Root}, Env: map[string]string{"MCP_KEY": "WORKSPACE_TEST_KEY"},
	}}
	raw := json.RawMessage(`{"server":"local","tool":"test_tool","arguments":{"value":42}}`)
	result, err := e.Execute(context.Background(), "mcp.call", raw)
	if err != nil || !bytes.Contains(result, []byte("mcp ok")) {
		t.Fatalf("forward: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(e.Config.Root, ".fake-mcp-record"))
	var messages []codexMessage
	if json.Unmarshal(data, &messages) != nil || len(messages) != 3 ||
		messages[0].Method != "initialize" || messages[1].Method != "notifications/initialized" ||
		messages[2].Method != "tools/call" || !bytes.Contains(messages[2].Params, []byte(`"value":42`)) {
		t.Fatal("MCP helper handshake/arguments were not forwarded")
	}
	for _, raw := range []string{
		`{"server":"https://attacker.test","tool":"test","arguments":{}}`,
		`{"server":"local","tool":"test","arguments":{},"command":"sh"}`,
		`{"server":"local","tool":"test","arguments":[]}`,
		`{"server":"local","tool":"test","arguments":null}`,
	} {
		if _, err := e.Execute(context.Background(), "mcp.call", json.RawMessage(raw)); err == nil {
			t.Fatal("unsafe MCP arguments accepted")
		}
	}
	_, err = e.Execute(context.Background(), "mcp.call", json.RawMessage(`{"server":"local","tool":"echo-secret","arguments":{}}`))
	if !errors.Is(err, ErrRuntimeFailed) {
		t.Fatal("MCP returned local environment secret", err)
	}
	e.Config.AllowedCapabilities = nil
	if _, err := e.Execute(context.Background(), "mcp.call", raw); !errors.Is(err, ErrCapabilityDenied) {
		t.Fatal(err)
	}
}
