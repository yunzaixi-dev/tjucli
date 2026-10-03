package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yunzaixi-dev/tjucli/internal/workspaceconfig"
)

func runCommand(t *testing.T, dir, input string, args ...string) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := (runner{in: strings.NewReader(input), out: &out, errOut: &errOut}).run(context.Background(), append([]string{"--config-dir", dir}, args...))
	if !json.Valid(out.Bytes()) {
		t.Fatalf("not an envelope: %s", out.String())
	}
	return code, out.String()
}

func TestWorkspaceLifecycleAndRedaction(t *testing.T) {
	dir, root := filepath.Join(t.TempDir(), "state"), t.TempDir()
	code, body := runCommand(t, dir, "", "workspace", "init", "--root", root, "--name", "测试宿主机")
	if code != 0 {
		t.Fatalf("init failed: %s", body)
	}
	code, body = runCommand(t, dir, "very-secret-token\n", "workspace", "link", "--api", "https://example.com/api", "--id", "workspace-one")
	if code != 0 || strings.Contains(body, "very-secret-token") {
		t.Fatalf("link unsafe: %s", body)
	}
	code, body = runCommand(t, dir, "", "workspace", "status")
	if code != 0 || strings.Contains(body, "very-secret-token") {
		t.Fatalf("status unsafe: %s", body)
	}
	code, body = runCommand(t, dir, "", "workspace", "allow", "arbitrary.shell")
	if code == 0 {
		t.Fatalf("arbitrary capability accepted: %s", body)
	}
	code, body = runCommand(t, dir, "", "workspace", "allow")
	if code != 0 {
		t.Fatalf("revoke failed: %s", body)
	}
	code, body = runCommand(t, dir, "", "workspace", "unlink")
	if code != 0 {
		t.Fatalf("unlink failed: %s", body)
	}
	code, _ = runCommand(t, dir, "", "connect", "--once")
	if code == 0 {
		t.Fatal("unlinked workspace connected")
	}
}

func TestInitializationDoesNotReplaceExistingWorkspace(t *testing.T) {
	dir, root := filepath.Join(t.TempDir(), "state"), t.TempDir()
	code, _ := runCommand(t, dir, "", "workspace", "init", "--root", root, "--name", "Original")
	if code != 0 {
		t.Fatal("init failed")
	}
	code, _ = runCommand(t, dir, "", "workspace", "init", "--root", root, "--name", "Replacement")
	if code == 0 {
		t.Fatal("init replaced an existing identity")
	}
}

func TestLinkAcceptsOpaqueWorkspaceIDPrefixes(t *testing.T) {
	for _, prefix := range []string{"-", "_"} {
		t.Run(prefix, func(t *testing.T) {
			dir, root := filepath.Join(t.TempDir(), "state"), t.TempDir()
			if code, _ := runCommand(t, dir, "", "workspace", "init", "--root", root, "--name", "Local"); code != 0 {
				t.Fatal("init failed")
			}
			id := prefix + strings.Repeat("A", 42)
			code, body := runCommand(t, dir, "synthetic-connection-token\n", "workspace", "link",
				"--api", "https://example.test/api", "--id", id)
			if code != 0 {
				t.Fatalf("opaque server ID rejected: %s", body)
			}
			config, err := workspaceconfig.Load(dir)
			if err != nil || config.ID != id {
				t.Fatal("opaque server ID not preserved")
			}
		})
	}
}

func TestLocalCapabilityPlatformGate(t *testing.T) {
	dir, root := filepath.Join(t.TempDir(), "state"), t.TempDir()
	if code, _ := runCommand(t, dir, "", "workspace", "init", "--root", root, "--name", "Local"); code != 0 {
		t.Fatal("init failed")
	}
	before, err := os.ReadFile(filepath.Join(dir, workspaceconfig.ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	var out, warning bytes.Buffer
	code := (runner{in: strings.NewReader(""), out: &out, errOut: &warning}).run(
		context.Background(), []string{"--config-dir", dir, "workspace", "allow", "pi.prompt"})
	if remoteExecutionSupported {
		if code != 0 || warning.Len() == 0 {
			t.Fatal("supported platform did not require a visible host-access warning")
		}
	} else {
		after, err := os.ReadFile(filepath.Join(dir, workspaceconfig.ConfigFileName))
		if code == 0 || !strings.Contains(out.String(), "workspace_runtime_unavailable") ||
			warning.Len() != 0 || err != nil || !bytes.Equal(before, after) {
			t.Fatal("unsupported platform reached capability mutation or warning")
		}
	}
	if code, body := runCommand(t, dir, "", "workspace", "allow"); code != 0 {
		t.Fatalf("revoke unavailable: %s", body)
	}
}

func TestDefaultDenyAndStrictConfiguration(t *testing.T) {
	dir, root := filepath.Join(t.TempDir(), "state"), t.TempDir()
	_, _ = runCommand(t, dir, "", "workspace", "init", "--root", root, "--name", "Local")
	// No installed model executable or developer auth may be used by this test.
	code, body := runCommand(t, dir, `{"prompt":"must not execute"}`, "run", "pi.prompt")
	if code == 0 || !strings.Contains(body, "workspace_execution_failed") {
		t.Fatalf("default-deny failed: %s", body)
	}
	code, _ = runCommand(t, dir, `{"connection_token":"injected"}`, "workspace", "configure")
	if code == 0 {
		t.Fatal("configure accepted connection credential injection")
	}
	code, body = runCommand(t, dir, `{"endpoints":[{"name":"local","base_url":"http://127.0.0.1:9999/v1","model":"test","api_key_env":"TJUCLAW_TEST_KEY"}]}`, "workspace", "configure")
	if code != 0 {
		t.Fatalf("local endpoint failed: %s", body)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitRuntimeAuthConfigurationIsPrivateAndFieldScoped(t *testing.T) {
	dir, root := filepath.Join(t.TempDir(), "state"), t.TempDir()
	_, _ = runCommand(t, dir, "", "workspace", "init", "--root", root, "--name", "Host attachment")
	code, body := runCommand(t, dir, `{"runtime_auth":{"claude":{"mode":"oauth-env","source":"MY_HOST_CLAUDE_TOKEN","model":"owner-model"}}}`, "workspace", "configure")
	if code != 0 || strings.Contains(body, "MY_HOST_CLAUDE_TOKEN") {
		t.Fatalf("auth attachment rejected/exposed source: %s", body)
	}
	config, err := workspaceconfig.Load(dir)
	if err != nil || config.RuntimeAuth["claude"].Source != "MY_HOST_CLAUDE_TOKEN" ||
		len(config.AllowedCapabilities) != 0 || config.ConnectionToken != "" {
		t.Fatalf("unexpected attachment mutation: %v", err)
	}
	code, _ = runCommand(t, dir, `{"plugins":[]}`, "workspace", "configure")
	config, err = workspaceconfig.Load(dir)
	if code != 0 || err != nil || config.RuntimeAuth["claude"].Source != "MY_HOST_CLAUDE_TOKEN" {
		t.Fatalf("unrelated configure replaced auth attachment: %v", err)
	}
	code, _ = runCommand(t, dir, `{"runtime_auth":{}}`, "workspace", "configure")
	config, err = workspaceconfig.Load(dir)
	if code != 0 || err != nil || len(config.RuntimeAuth) != 0 {
		t.Fatalf("could not detach owner-selected auth: %v", err)
	}
}

func TestExplicitConfigDoesNotRequireHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	dir, root := filepath.Join(t.TempDir(), "state"), t.TempDir()
	code, body := runCommand(t, dir, "", "workspace", "init", "--root", root, "--name", "Native")
	if code != 0 {
		t.Fatalf("explicit config depends on HOME: %s", body)
	}
}

func TestLocalOwnerMetadataAndApprovalCommands(t *testing.T) {
	dir, root := filepath.Join(t.TempDir(), "state"), t.TempDir()
	_, _ = runCommand(t, dir, "", "workspace", "init", "--root", root, "--name", "Local owner")
	for _, command := range []string{"sessions", "approvals"} {
		code, body := runCommand(t, dir, "", command)
		if code != 0 || !strings.Contains(body, `"`+command+`":[]`) {
			t.Fatalf("empty local metadata %s: %s", command, body)
		}
	}
	for _, args := range [][]string{
		{"approvals", "extra"},
		{"approval", "../request", "allow"},
		{"approval", "missing", "always"},
		{"session", "missing"},
		{"sessions", "--runtime", "arbitrary"},
	} {
		code, body := runCommand(t, dir, "", args...)
		if code == 0 || strings.Contains(body, root) || strings.Contains(body, dir) {
			t.Fatalf("unsafe local metadata command %v: %s", args, body)
		}
	}
	// Approval responses must never become remotely enabled capabilities.
	code, _ := runCommand(t, dir, "", "workspace", "allow", "approval")
	if code == 0 {
		t.Fatal("owner approval exposed as remote capability")
	}
}

func TestLocalMetadataEnvelopeBound(t *testing.T) {
	var out bytes.Buffer
	code := (runner{out: &out}).respondBounded(map[string]string{"input": strings.Repeat("x", 1<<20)})
	if code == 0 || !strings.Contains(out.String(), "workspace_result_invalid") {
		t.Fatal("oversized metadata escaped native envelope bound")
	}
}

func TestConfigureRejectsAmbiguousJSON(t *testing.T) {
	dir, root := filepath.Join(t.TempDir(), "state"), t.TempDir()
	_, _ = runCommand(t, dir, "", "workspace", "init", "--root", root, "--name", "Strict")
	for _, body := range []string{`null`, `[]`, `{"plugins":[],"plugins":[]}`, `{"endpoints":null}`,
		`{"mcp_servers":{"peer":{"command":"test","command":"other"}}}`} {
		code, output := runCommand(t, dir, body, "workspace", "configure")
		if code == 0 {
			t.Fatalf("accepted ambiguous configuration: %s", output)
		}
	}
}

func TestConfigureMCPServerReferencesAreAccepted(t *testing.T) {
	dir, root := filepath.Join(t.TempDir(), "state"), t.TempDir()
	if code, _ := runCommand(t, dir, "", "workspace", "init", "--root", root, "--name", "Local"); code != 0 {
		t.Fatal("init failed")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]any{"mcp_servers": map[string]any{"test": map[string]any{
		"command": executable, "args": []string{"-test.run=^TestWorkspaceMCPProcess$"},
		"env": map[string]string{"TJUCLAW_MCP_INTEGRATION": "TJUCLAW_MCP_INTEGRATION"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if code, body := runCommand(t, dir, string(input), "workspace", "configure"); code != 0 {
		t.Fatalf("MCP configuration rejected: %s", body)
	}
	config, err := workspaceconfig.Load(dir)
	if err != nil || config.MCPServers["test"].Command != executable {
		t.Fatal("MCP executable not preserved")
	}
}

type gatedReader struct {
	started chan struct{}
	release chan struct{}
	done    bool
	body    string
	offset  int
}

func (r *gatedReader) Read(body []byte) (int, error) {
	if !r.done {
		r.done = true
		close(r.started)
		<-r.release
	}
	input := r.body
	if input == "" {
		input = "{}"
	}
	if r.offset >= len(input) {
		return 0, io.EOF
	}
	n := copy(body, input[r.offset:])
	r.offset += n
	return n, nil
}

func TestAdditiveConfigureUsesCurrentLists(t *testing.T) {
	dir, root := filepath.Join(t.TempDir(), "state"), t.TempDir()
	_, _ = runCommand(t, dir, "", "workspace", "init", "--root", root, "--name", "Additive")
	first, second := filepath.Join(root, "first.ts"), filepath.Join(root, "second.ts")
	initial, _ := json.Marshal(map[string]any{
		"endpoints": []workspaceconfig.Endpoint{{Name: "hidden", BaseURL: "https://existing.example/v1", Model: "one", APIKeyEnv: "HIDDEN_KEY"}},
		"plugins":   []string{first},
	})
	if code, _ := runCommand(t, dir, string(initial), "workspace", "configure"); code != 0 {
		t.Fatal("initial config rejected")
	}
	draft, _ := json.Marshal(map[string]any{
		"endpoints": []map[string]string{{"name": "new", "base_url": "https://new.example/v1", "model": "two"}},
		"plugins":   []string{first},
	})
	input := &gatedReader{started: make(chan struct{}), release: make(chan struct{}), body: string(draft)}
	finished := make(chan int, 1)
	go func() {
		var output bytes.Buffer
		finished <- (runner{in: input, out: &output, errOut: io.Discard}).run(context.Background(), []string{"--config-dir", dir, "workspace", "configure", "--merge"})
	}()
	select {
	case <-input.started:
	case <-time.After(5 * time.Second):
		t.Fatal("merge did not read input")
	}
	concurrent, _ := json.Marshal(map[string]any{"plugins": []string{first, second}})
	if code, _ := runCommand(t, dir, string(concurrent), "workspace", "configure"); code != 0 {
		t.Fatal("concurrent config failed")
	}
	close(input.release)
	select {
	case code := <-finished:
		if code != 0 {
			t.Fatal("merge failed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("merge hung")
	}
	config, err := workspaceconfig.Load(dir)
	if err != nil || len(config.Endpoints) != 2 || config.Endpoints[0].APIKeyEnv != "HIDDEN_KEY" ||
		len(config.Plugins) != 2 || config.Plugins[1] != second {
		t.Fatal("additive draft dropped hidden/current references")
	}
	code, _ := runCommand(t, dir, `{"endpoints":[{"name":"new","base_url":"https://new.example/v1","model":"three"}],"plugins":[]}`, "workspace", "configure", "--merge")
	config, err = workspaceconfig.Load(dir)
	if code != 0 || err != nil || len(config.Endpoints) != 2 || config.Endpoints[1].Model != "three" || len(config.Plugins) != 2 {
		t.Fatal("merge did not upsert or empty append removed plugins")
	}
	for _, body := range []string{
		`{"runtime_auth":{}}`, `{"mcp_servers":{}}`,
		`{"endpoints":[{"name":"new","base_url":"https://new.example/v1","model":"x"},{"name":"new","base_url":"https://new.example/v1","model":"y"}]}`,
	} {
		if code, _ := runCommand(t, dir, body, "workspace", "configure", "--merge"); code == 0 {
			t.Fatal("ambiguous/advanced additive payload accepted")
		}
	}
}

func TestEndpointCredentialReferencePreservedOnlyFromCurrentBinding(t *testing.T) {
	dir, root := filepath.Join(t.TempDir(), "state"), t.TempDir()
	_, _ = runCommand(t, dir, "", "workspace", "init", "--root", root, "--name", "References")
	original := `{"endpoints":[{"name":"model","base_url":"https://example.com/v1","model":"one","api_key_env":"HOST_MODEL_KEY"}]}`
	if code, _ := runCommand(t, dir, original, "workspace", "configure"); code != 0 {
		t.Fatal("reference fixture failed")
	}
	draft := `{"endpoints":[{"name":"model","base_url":"https://example.com/v1","model":"two"}]}`
	if code, _ := runCommand(t, dir, draft, "workspace", "configure"); code != 0 {
		t.Fatal("endpoint draft failed")
	}
	config, _ := workspaceconfig.Load(dir)
	if config.Endpoints[0].APIKeyEnv != "HOST_MODEL_KEY" {
		t.Fatal("unchanged binding lost reference")
	}
	input := &gatedReader{started: make(chan struct{}), release: make(chan struct{}), body: draft}
	finished := make(chan int, 1)
	go func() {
		var output bytes.Buffer
		finished <- (runner{in: input, out: &output, errOut: io.Discard}).run(context.Background(), []string{"--config-dir", dir, "workspace", "configure"})
	}()
	select {
	case <-input.started:
	case <-time.After(5 * time.Second):
		t.Fatal("draft never read stdin")
	}
	revoke := `{"endpoints":[{"name":"model","base_url":"https://example.com/v1","model":"one","api_key_env":""}]}`
	if code, _ := runCommand(t, dir, revoke, "workspace", "configure"); code != 0 {
		t.Fatal("reference revoke failed")
	}
	close(input.release)
	select {
	case code := <-finished:
		if code != 0 {
			t.Fatal("pending draft failed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending draft hung")
	}
	config, _ = workspaceconfig.Load(dir)
	if config.Endpoints[0].APIKeyEnv != "" {
		t.Fatal("pending draft resurrected revoked reference")
	}
	_, _ = runCommand(t, dir, original, "workspace", "configure")
	retarget := `{"endpoints":[{"name":"model","base_url":"https://different.example/v1","model":"two"}]}`
	if code, _ := runCommand(t, dir, retarget, "workspace", "configure"); code != 0 {
		t.Fatal("endpoint retarget failed")
	}
	config, _ = workspaceconfig.Load(dir)
	if config.Endpoints[0].APIKeyEnv != "" {
		t.Fatal("credential silently retargeted to another endpoint")
	}
}

func TestPendingConfigureDoesNotRestoreRevocation(t *testing.T) {
	dir, root := filepath.Join(t.TempDir(), "state"), t.TempDir()
	_, _ = runCommand(t, dir, "", "workspace", "init", "--root", root, "--name", "Concurrent")
	_, _ = runCommand(t, dir, "secret-token", "workspace", "link", "--api", "https://example.com/api", "--id", "registered")
	_, _ = runCommand(t, dir, "", "workspace", "allow", "pi.prompt")
	input := &gatedReader{started: make(chan struct{}), release: make(chan struct{})}
	finished := make(chan int, 1)
	go func() {
		var output bytes.Buffer
		finished <- (runner{in: input, out: &output, errOut: io.Discard}).run(context.Background(), []string{"--config-dir", dir, "workspace", "configure"})
	}()
	select {
	case <-input.started:
	case <-time.After(5 * time.Second):
		t.Fatal("configure never read stdin")
	}
	_, _ = runCommand(t, dir, "", "workspace", "allow")
	_, _ = runCommand(t, dir, "", "workspace", "unlink")
	close(input.release)
	select {
	case code := <-finished:
		if code != 0 {
			t.Fatal("configure failed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("configure did not finish")
	}
	config, err := workspaceconfig.Load(dir)
	if err != nil || config.ConnectionToken != "" || len(config.AllowedCapabilities) != 0 {
		t.Fatalf("pending configure restored revoked permissions: load_error=%v", err)
	}
}
