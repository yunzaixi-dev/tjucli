package workspaceruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yunzaixi-dev/tjucli/internal/workspaceconfig"
)

func fakeBridgeCLI() {
	if len(os.Args) < 4 {
		os.Exit(2)
	}
	cwd, _ := os.Getwd()
	input, _ := io.ReadAll(io.LimitReader(os.Stdin, maxArguments+1))
	record, _ := json.Marshal(processRecord{Args: os.Args[1:], CWD: cwd, Env: os.Environ(), Input: string(input)})
	f, _ := os.OpenFile(filepath.Join(cwd, ".fake-cli-record"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if f != nil {
		_, _ = f.Write(append(record, '\n'))
		_ = f.Close()
	}
	mode, _ := os.ReadFile(filepath.Join(cwd, ".fake-cli-mode"))
	switch string(mode) {
	case "failure":
		fmt.Println(`{"ok":false,"error":{"id":"private fake-token fake-secret"}}`)
		os.Exit(1)
	case "malformed":
		fmt.Println("not JSON fake-token fake-secret")
		return
	case "hang":
		time.Sleep(10 * time.Minute)
	case "flood":
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), maxOutput+8192))
		time.Sleep(10 * time.Minute)
	case "echo-secret":
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": true, "data": map[string]any{
			"content": []any{map[string]string{"type": "text", "text": os.Getenv("BRIDGE_MCP_HOST")}},
		}})
		return
	}
	var data any
	switch os.Args[3] {
	case "workspaces":
		data = map[string]any{"workspaces": []any{map[string]any{
			"id": "target", "name": "环境", "online": true, "capabilities": []string{"pi.prompt"},
			"connection_token": "fake-token", "owner": "private-owner",
		}}}
	case "invoke":
		data = map[string]any{"id": "call", "status": "queued", "connection_token": "fake-token",
			"arguments": map[string]string{"hidden": "private-input"}}
	case "invocation":
		data = map[string]any{"id": "call", "status": "succeeded", "result": map[string]string{"output": "remote result"},
			"connection_token": "fake-token"}
	case "run":
		if len(os.Args) != 5 || os.Args[4] != "mcp.call" {
			os.Exit(2)
		}
		var config workspaceconfig.Config
		raw, _ := os.ReadFile(filepath.Join(cwd, ".fake-cli-config"))
		if json.Unmarshal(raw, &config) != nil {
			os.Exit(2)
		}
		result, err := (Executor{Config: config, StateDir: os.Args[2]}).Execute(context.Background(), "mcp.call", input)
		if err != nil {
			fmt.Println(`{"ok":false,"error":{"id":"workspace_execution_failed"}}`)
			return
		}
		data = result
	default:
		os.Exit(2)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": true, "data": data})
}

func TestKeylessEndpointsDoNotInheritAccounts(t *testing.T) {
	for _, capability := range []string{"pi.prompt", "claude.prompt", "codex.prompt"} {
		t.Run(capability, func(t *testing.T) {
			e := fakeExecutor(t, capability)
			e.Config.Endpoints[0].APIKeyEnv = ""
			result, err := e.Execute(context.Background(), capability, json.RawMessage(`{"prompt":"ok"}`))
			if err != nil || !bytes.Contains(result, []byte(`"output":"ok"`)) {
				t.Fatal("keyless execution", err)
			}
			record := readRecord(t, e)
			for _, entry := range record.Env {
				if strings.Contains(entry, "fake-secret") || strings.Contains(entry, "unrelated-developer-secret") {
					t.Fatal("keyless endpoint inherited credentials")
				}
			}
			if capability == "pi.prompt" && !strings.Contains(record.Files["models.json"], `"apiKey":"workspace-keyless"`) {
				t.Fatal("Pi local placeholder missing")
			}
			if capability == "codex.prompt" && strings.Contains(record.Files["config.toml"], "env_key") {
				t.Fatal("Codex keyless provider requires auth")
			}
		})
	}
}

func TestPiBridgeOnlyExposedWhenLinked(t *testing.T) {
	e := fakeExecutor(t, "pi.prompt")
	if _, err := e.Execute(context.Background(), "pi.prompt", json.RawMessage(`{"prompt":"ok"}`)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(readRecord(t, e).Args, " "), "workspace-bridge.ts") {
		t.Fatal("unlinked workspace exposed remote tools")
	}
	e.Config.APIBaseURL, e.Config.ConnectionToken = "https://app.example.test/api", "fake-connection-token"
	if _, err := e.Execute(context.Background(), "pi.prompt", json.RawMessage(`{"prompt":"ok"}`)); err != nil {
		t.Fatal(err)
	}
	record := readRecord(t, e)
	source := record.Files["workspace-bridge.ts"]
	if source == "" || !strings.Contains(source, "workspace_invoke") || !strings.Contains(source, e.StateDir) {
		t.Fatal("linked workspace missing built-in extension")
	}
	if strings.Contains(source, "fake-connection-token") || strings.Contains(source, "fake-secret") ||
		strings.Contains(source, e.Config.APIBaseURL) || slicesContain(record.Args, "--no-tools") {
		t.Fatal("extension embeds credentials or disables full host tools")
	}
}

func TestPiMCPBridgePermissionAndIsolation(t *testing.T) {
	for _, test := range []struct {
		name    string
		allowed bool
		server  bool
		want    bool
	}{
		{"no-permission", false, true, false},
		{"no-server", true, false, false},
		{"standalone", true, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := fakeExecutor(t, "pi.prompt")
			t.Setenv("BRIDGE_MCP_HOST", "mcp-private-value")
			if test.allowed {
				e.Config.AllowedCapabilities = append(e.Config.AllowedCapabilities, "mcp.call")
			}
			if test.server {
				e.Config.MCPServers = map[string]workspaceconfig.MCPServer{
					"local.demo": {Command: "fake", Env: map[string]string{"MCP_KEY": "BRIDGE_MCP_HOST"}},
				}
			}
			if _, err := e.Execute(context.Background(), "pi.prompt", json.RawMessage(`{"prompt":"ok"}`)); err != nil {
				t.Fatal(err)
			}
			record := readRecord(t, e)
			source := record.Files["workspace-bridge.ts"]
			if (source != "") != test.want {
				t.Fatal("MCP registration gate")
			}
			if strings.Contains(source, "mcp-private-value") || strings.Contains(strings.Join(record.Env, " "), "mcp-private-value") {
				t.Fatal("MCP secret embedded in source or Pi environment")
			}
			if test.want && !strings.Contains(source, "const linked = false;") {
				t.Fatal("standalone bridge exposed linked tools")
			}
			if test.want {
				setMode(t, e, "echo-mcp-secret")
				result, err := e.Execute(context.Background(), "pi.prompt", json.RawMessage(`{"prompt":"ok"}`))
				if err != nil || bytes.Contains(result, []byte("mcp-private-value")) || !bytes.Contains(result, []byte("[redacted]")) {
					t.Fatal("Pi completion leaked MCP credential", err)
				}
			}
		})
	}
}

func slicesContain(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

// This harness tests the generated tool handlers with a fake registerTool API,
// fake CLI subprocess, and tiny schema stub. It does not claim live Pi/model
// integration. The actual API/imports were checked in installed Pi docs.
func TestGeneratedPiBridgeTools(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable for generated extension handler test")
	}
	e := fakeExecutor(t, "pi.prompt", "mcp.call")
	e.Config.APIBaseURL, e.Config.ConnectionToken = "https://app.example.test/api", "fake-connection-token"
	t.Setenv("BRIDGE_MCP_HOST", "private-mcp-\"value\nline")
	t.Setenv("BRIDGE_OTHER_HOST", "other-mcp-private-value")
	self, _ := os.Executable()
	e.Config.MCPServers = map[string]workspaceconfig.MCPServer{
		"local.demo": {Command: self, Args: []string{"--fake-mcp", e.Config.Root},
			Env: map[string]string{"MCP_KEY": "BRIDGE_MCP_HOST"}},
		"other": {Command: self, Args: []string{"--fake-mcp", e.Config.Root},
			Env: map[string]string{"MCP_KEY": "BRIDGE_OTHER_HOST"}},
	}
	if workspaceconfig.EnsurePrivateDir(e.Config.Root) != nil {
		t.Fatal("private fake CLI fixture directory")
	}
	if privateJSON(filepath.Join(e.Config.Root, ".fake-cli-config"), e.Config) != nil {
		t.Fatal("fake CLI config")
	}
	state, err := e.prepare()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(state.runDir)
	path, err := e.piBridge(state)
	if err != nil {
		t.Fatal(err)
	}
	source, _ := os.ReadFile(path)
	if strings.Contains(string(source), os.Getenv("BRIDGE_MCP_HOST")) ||
		strings.Contains(string(source), "other-mcp-private-value") {
		t.Fatal("secret embedded in extension source")
	}
	info, err := os.Stat(filepath.Join(state.runDir, "pi", "mcp-env.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("MCP environment transport is not private")
	}
	// Avoid any dependency install or accessing a developer's Pi profile.
	stub := filepath.Join(state.runDir, "typebox.mjs")
	if os.WriteFile(stub, []byte(`export const Type = {
 Object: (properties, options) => ({ type: "object", properties, ...options }),
 String: (options) => ({ type: "string", ...options }),
 Record: () => ({ type: "object" }), Unknown: () => ({})
};`), 0600) != nil {
		t.Fatal("write schema stub")
	}
	testSource := strings.Replace(string(source), `import { Type } from "typebox";`, `import { Type } from "./typebox.mjs";`, 1)
	module := filepath.Join(state.runDir, "extension.mjs")
	if os.WriteFile(module, []byte(testSource), 0600) != nil {
		t.Fatal("write extension fixture")
	}
	harness := filepath.Join(state.runDir, "harness.mjs")
	script := `
import bridge from "./extension.mjs";
import assert from "node:assert/strict";
import { writeFile, readFile } from "node:fs/promises";
const tools = new Map();
bridge({ registerTool: (tool) => tools.set(tool.name, tool) });
assert.deepEqual([...tools.keys()], ["workspace_mcp_call", "workspace_list", "workspace_invoke", "workspace_invocation"]);
const controller = new AbortController();
const exec = async (name, args, signal = controller.signal) =>
 JSON.parse((await tools.get(name).execute("id", args, signal)).content[0].text);
const list = await exec("workspace_list", {});
assert.equal(list.data.workspaces[0].id, "target");
assert.equal(list.data.workspaces[0].connection_token, undefined);
assert.equal(list.data.workspaces[0].owner, undefined);
const invocation = await exec("workspace_invoke", { target: "target", capability: "pi.prompt", arguments: { prompt: "--flags are text" } });
assert.equal(invocation.data.status, "queued");
assert.equal(invocation.data.arguments, undefined);
const done = await exec("workspace_invocation", { id: "call" });
assert.equal(done.data.result.output, "remote result");
assert.equal(done.data.connection_token, undefined);
assert.deepEqual(tools.get("workspace_mcp_call").parameters.properties.server.enum, ["local.demo", "other"]);
const mcp = await exec("workspace_mcp_call", { server: "local.demo", tool: "echo", args: { text: "--literal" } });
assert.equal(mcp.ok, true);
assert.equal(mcp.data.content[0].text, "mcp ok");
const serverEnv = JSON.parse(await readFile(".fake-mcp-env", "utf8"));
assert(serverEnv.includes('MCP_KEY=private-mcp-"value\nline'));
assert(!serverEnv.some(value => value.startsWith("BRIDGE_MCP_HOST=") || value.startsWith("BRIDGE_OTHER_HOST=") ||
 value.startsWith("OPENAI_API_KEY=") || value.startsWith("NODE_OPTIONS=")));
const configText = await readFile(".fake-cli-config", "utf8");
const revoked = JSON.parse(configText);
revoked.allowed_capabilities = ["pi.prompt"];
await writeFile(".fake-cli-config", JSON.stringify(revoked));
await assert.rejects(exec("workspace_mcp_call", { server: "local.demo", tool: "echo", args: {} }), /workspace_request_failed/);
await writeFile(".fake-cli-config", configText);
await assert.rejects(exec("workspace_mcp_call", { server: "unconfigured", tool: "echo", args: {} }), /workspace_invalid_arguments/);
await assert.rejects(exec("workspace_mcp_call", { server: "local.demo", tool: "echo", args: {}, command: "sh" }), /workspace_invalid_arguments/);
await assert.rejects(exec("workspace_mcp_call", { server: "local.demo", tool: "echo\nbad", args: {} }), /workspace_invalid_arguments/);
await assert.rejects(exec("workspace_mcp_call", { server: "local.demo", tool: "echo", args: [] }), /workspace_invalid_arguments/);
await assert.rejects(exec("workspace_mcp_call", { server: "local.demo", tool: "echo", args: { text: "x".repeat(65536) } }), /workspace_invalid_arguments/);
await assert.rejects(exec("workspace_mcp_call", { server: "local.demo", tool: "echo-secret", args: {} }), /workspace_request_failed/);
await writeFile(".fake-cli-mode", "echo-secret");
await assert.rejects(exec("workspace_mcp_call", { server: "local.demo", tool: "echo", args: {} }), /workspace_request_failed/);
await writeFile(".fake-cli-mode", "");
await assert.rejects(exec("workspace_list", { command: "sh" }), /workspace_invalid_arguments/);
await assert.rejects(exec("workspace_invoke", { target: "--evil", capability: "pi.prompt", arguments: {} }), /workspace_invalid_arguments/);
await assert.rejects(exec("workspace_invoke", { target: "target", capability: "shell", arguments: {} }), /workspace_invalid_arguments/);
await assert.rejects(exec("workspace_invoke", { target: "target", capability: "pi.prompt", arguments: { prompt: "x".repeat(65536) } }), /workspace_invalid_arguments/);
await assert.rejects(exec("workspace_invocation", { id: "call", args: [] }), /workspace_invalid_arguments/);
for (const [mode, error] of [["failure", "workspace_request_failed"], ["malformed", "workspace_response_invalid"], ["flood", "workspace_response_limit"]]) {
 await writeFile(".fake-cli-mode", mode);
 await assert.rejects(exec("workspace_list", {}), new RegExp(error));
}

await writeFile(".fake-cli-mode", "hang");
const canceled = new AbortController();
setTimeout(() => canceled.abort(), 100);
await assert.rejects(exec("workspace_list", {}, canceled.signal), /workspace_request_canceled/);
console.log("bridge handlers ok");
`
	if os.WriteFile(harness, []byte(script), 0600) != nil {
		t.Fatal("write harness")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, harness)
	cmd.Dir, cmd.Env = e.Config.Root, append(state.env, keyVariable+"=fake-secret", "ANTHROPIC_API_KEY=unrelated-developer-secret")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated extension handlers: %v\n%s", err, output)
	}
	data, _ := os.ReadFile(filepath.Join(e.Config.Root, ".fake-cli-record"))
	for _, raw := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var record processRecord
		if json.Unmarshal(raw, &record) != nil || len(record.Args) < 3 || record.Args[0] != "--config-dir" || record.Args[1] != e.StateDir {
			t.Fatal("bridge did not use fixed CLI coordinates")
		}
		if strings.Contains(strings.Join(record.Env, " "), "fake-secret") || strings.Contains(strings.Join(record.Env, " "), "unrelated-developer-secret") {
			t.Fatal("bridge CLI inherited model credentials")
		}
		if record.Args[2] == "invoke" && record.Input != `{"prompt":"--flags are text"}` {
			t.Fatal("bridge invocation arguments were not sent as stdin JSON")
		}
		if record.CWD != e.Config.Root {
			t.Fatal("bridge cwd changed")
		}
		if record.Args[2] == "run" {
			if len(record.Args) != 4 || record.Args[3] != "mcp.call" ||
				envValue(record.Env, "BRIDGE_MCP_HOST") != os.Getenv("BRIDGE_MCP_HOST") ||
				envValue(record.Env, "BRIDGE_OTHER_HOST") != "" ||
				envValue(record.Env, "NODE_OPTIONS") != "" {
				t.Fatal("MCP CLI transport did not isolate selected approved refs")
			}
			if strings.Contains(strings.Join(record.Args, " "), "private-mcp") {
				t.Fatal("MCP credential in argv")
			}
			if record.Input != `{"server":"local.demo","tool":"echo","arguments":{"text":"--literal"}}` &&
				record.Input != `{"server":"local.demo","tool":"echo-secret","arguments":{}}` &&
				record.Input != `{"server":"local.demo","tool":"echo","arguments":{}}` {
				t.Fatal("MCP call was not fixed stdin JSON")
			}
		} else if envValue(record.Env, "BRIDGE_MCP_HOST") != "" || envValue(record.Env, "BRIDGE_OTHER_HOST") != "" {
			t.Fatal("cross-workspace CLI inherited MCP credentials")
		}
	}
	// Unlinked environments register only the independent MCP tool.
	standalone := strings.Replace(testSource, "const linked = true;", "const linked = false;", 1)
	standaloneModule := filepath.Join(state.runDir, "standalone.mjs")
	if os.WriteFile(standaloneModule, []byte(standalone), 0600) != nil {
		t.Fatal("standalone extension")
	}
	standaloneHarness := filepath.Join(state.runDir, "standalone-harness.mjs")
	if os.WriteFile(standaloneHarness, []byte(`
import bridge from "./standalone.mjs";
import assert from "node:assert/strict";
const tools = [];
bridge({ registerTool: tool => tools.push(tool) });
assert.deepEqual(tools.map(tool => tool.name), ["workspace_mcp_call"]);
`), 0600) != nil {
		t.Fatal("standalone harness")
	}
	cmd = exec.CommandContext(ctx, node, standaloneHarness)
	cmd.Dir, cmd.Env = e.Config.Root, state.env
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("standalone registration: %v\n%s", err, output)
	}
	// An approved-but-unset host ref fails closed before spawning the CLI.
	if os.WriteFile(filepath.Join(state.runDir, "pi", "mcp-env.json"), []byte(`{"local.demo":{}}`), 0600) != nil {
		t.Fatal("unset ref fixture")
	}
	missingHarness := filepath.Join(state.runDir, "missing-harness.mjs")
	if os.WriteFile(missingHarness, []byte(`
import bridge from "./standalone.mjs";
import assert from "node:assert/strict";
let tool;
bridge({ registerTool: value => tool = value });
await assert.rejects(tool.execute("id", {server: "local.demo", tool: "echo", args: {}}), /workspace_request_failed/);
`), 0600) != nil {
		t.Fatal("missing ref harness")
	}
	cmd = exec.CommandContext(ctx, node, missingHarness)
	cmd.Dir, cmd.Env = e.Config.Root, state.env
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unset ref rejected: %v\n%s", err, output)
	}
}

func TestPiFullBuiltinsRetainPluginPermissionPolicies(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	path := filepath.Join(t.TempDir(), "host-tools.mjs")
	script := piHostToolsSource + `
const handlers = new Map();
let active = ["read", "owner_plugin"];
hostTools({
 on: (event, handler) => handlers.set(event, handler),
 getActiveTools: () => active,
 getAllTools: () => [
  ...["read", "bash", "edit", "write", "grep", "find", "ls"].map(name => ({name, sourceInfo: {source: "builtin"}})),
  {name: "owner_plugin", sourceInfo: {source: "extension"}},
  {name: "inactive_plugin", sourceInfo: {source: "extension"}}
 ],
 setActiveTools: names => active = names
});
handlers.get("session_start")();
if (!["read", "bash", "edit", "write", "grep", "find", "ls", "owner_plugin"].every(name => active.includes(name)) ||
 active.includes("inactive_plugin")) throw new Error("host tool policy");
`
	if os.WriteFile(path, []byte(script), 0600) != nil {
		t.Fatal("write host tools harness")
	}
	cmd := exec.Command(node, path)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("host tool activation: %v\n%s", err, output)
	}
}
