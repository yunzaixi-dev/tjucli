// tjuclaw connects an explicitly authorized system workspace to TJUClaw.
// It is separate from tjucli, the campus tools client.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/yunzaixi-dev/tjucli/internal/workspacebridge"
	"github.com/yunzaixi-dev/tjucli/internal/workspaceconfig"
	"github.com/yunzaixi-dev/tjucli/internal/workspaceruntime"
	"github.com/yunzaixi-dev/tjucli/internal/workspaceterminal"
)

var version = "dev"

type runner struct {
	in     io.Reader
	out    io.Writer
	errOut io.Writer
}

func (r runner) respond(data any, code string) int {
	if code != "" {
		_ = json.NewEncoder(r.out).Encode(map[string]any{"ok": false, "error": map[string]string{"id": code}})
		return 1
	}
	_ = json.NewEncoder(r.out).Encode(map[string]any{"ok": true, "data": data})
	return 0
}

func parseFlags(args []string, setup func(*flag.FlagSet)) (*flag.FlagSet, error) {
	fs := flag.NewFlagSet("tjuclaw", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	setup(fs)
	err := fs.Parse(args)
	if err == nil && len(fs.Args()) != 0 {
		err = errors.New("unexpected_arguments")
	}
	return fs, err
}

func (r runner) readJSON() (json.RawMessage, error) {
	body, err := io.ReadAll(io.LimitReader(r.in, (1<<20)+1))
	if err != nil || len(body) > 1<<20 || !json.Valid(body) {
		return nil, errors.New("invalid_json")
	}
	return json.RawMessage(body), nil
}

func (r runner) run(ctx context.Context, args []string) int {
	var dir string
	if len(args) >= 2 && args[0] == "--config-dir" {
		dir, args = args[1], args[2:]
	} else if len(args) != 0 && strings.HasPrefix(args[0], "--config-dir=") {
		dir, args = strings.TrimPrefix(args[0], "--config-dir="), args[1:]
	} else {
		base, err := os.UserConfigDir()
		if err != nil {
			return r.respond(nil, "workspace_config_unavailable")
		}
		dir = filepath.Join(base, "tjuclaw")
	}
	dir, err := filepath.Abs(dir)
	if err != nil || len(args) == 0 {
		return r.respond(nil, "usage_required")
	}
	if args[0] == "version" && len(args) == 1 {
		return r.respond(map[string]string{"version": version, "product": "tjuclaw"}, "")
	}
	if args[0] == "update" {
		return r.update(ctx, args[1:])
	}
	if args[0] == "workspace" {
		return r.workspace(dir, args[1:])
	}
	config, err := workspaceconfig.Load(dir)
	if err != nil {
		return r.respond(nil, "workspace_config_unavailable")
	}
	switch args[0] {
	case "sessions", "session", "approvals", "approval":
		// Local owner APIs only: never registered as connector/model capabilities.
		executor := workspaceruntime.Executor{Config: config, StateDir: dir}
		switch args[0] {
		case "sessions":
			var runtime string
			if _, err := parseFlags(args[1:], func(fs *flag.FlagSet) {
				fs.StringVar(&runtime, "runtime", "", "")
			}); err != nil {
				return r.respond(nil, "usage_required")
			}
			items, err := executor.ListSessions(ctx, runtime)
			if err != nil {
				return r.respond(nil, "workspace_session_unavailable")
			}
			return r.respondBounded(map[string]any{"sessions": items})
		case "session":
			if len(args) != 2 {
				return r.respond(nil, "usage_required")
			}
			item, err := executor.Session(ctx, args[1])
			if err != nil {
				return r.respond(nil, "workspace_session_unavailable")
			}
			return r.respondBounded(item)
		case "approvals":
			if len(args) != 1 {
				return r.respond(nil, "usage_required")
			}
			items, err := executor.PendingApprovals(ctx)
			if err != nil {
				return r.respond(nil, "workspace_approval_unavailable")
			}
			return r.respondBounded(map[string]any{"approvals": items})
		case "approval":
			if len(args) != 3 || (args[2] != "allow" && args[2] != "deny") {
				return r.respond(nil, "usage_required")
			}
			if err := executor.RespondApproval(ctx, args[1], workspaceruntime.ApprovalDecision(args[2])); err != nil {
				return r.respond(nil, "workspace_approval_unavailable")
			}
			return r.respond(map[string]bool{"responded": true}, "")
		}
	case "run":
		if len(args) != 2 {
			return r.respond(nil, "usage_required")
		}
		body, err := r.readJSON()
		if err != nil {
			return r.respond(nil, "invalid_json")
		}
		executor := workspaceruntime.Executor{Config: config, StateDir: dir}
		approvalDir, err := executor.ApprovalDirectory()
		if err != nil {
			return r.respond(nil, "workspace_execution_failed")
		}
		executor.Approvals = workspaceruntime.FileApprovals{Dir: approvalDir}
		runCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
		result, err := executor.Execute(runCtx, args[1], body)
		if err != nil {
			return r.respond(nil, "workspace_execution_failed")
		}
		if !json.Valid(result) || len(result) > 1<<20 {
			return r.respond(nil, "workspace_result_invalid")
		}
		return r.respond(result, "")
	case "connect", "invoke", "invocation", "workspaces":
		client, err := workspacebridge.New(config.APIBaseURL, config.ConnectionToken)
		if err != nil {
			return r.respond(nil, "workspace_not_linked")
		}
		switch args[0] {
		case "workspaces":
			if len(args) != 1 {
				return r.respond(nil, "usage_required")
			}
			workspaces, err := client.List(ctx)
			if err != nil {
				return r.respond(nil, safeRemoteCode(err))
			}
			return r.respond(map[string]any{"workspaces": workspaces}, "")
		case "connect":
			once := false
			if _, err := parseFlags(args[1:], func(fs *flag.FlagSet) { fs.BoolVar(&once, "once", false, "") }); err != nil {
				return r.respond(nil, "usage_required")
			}
			if err := r.connect(ctx, client, dir, config, once); err != nil {
				return r.respond(nil, "workspace_connection_failed")
			}
			return r.respond(map[string]bool{"stopped": true}, "")
		case "invoke":
			var timeoutSeconds int
			if len(args) < 3 {
				return r.respond(nil, "usage_required")
			}
			if _, err := parseFlags(args[3:], func(fs *flag.FlagSet) {
				fs.IntVar(&timeoutSeconds, "timeout-seconds", 0, "")
			}); err != nil || timeoutSeconds < 0 || timeoutSeconds > 86400 {
				return r.respond(nil, "usage_required")
			}
			body, err := r.readJSON()
			if err != nil {
				return r.respond(nil, "invalid_json")
			}
			invocation, err := client.InvokeWithTimeout(ctx, args[1], args[2], body, timeoutSeconds)
			if err != nil {
				return r.respond(nil, safeRemoteCode(err))
			}
			return r.respond(invocation, "")
		case "invocation":
			if len(args) != 2 {
				return r.respond(nil, "usage_required")
			}
			invocation, err := client.Get(ctx, args[1])
			if err != nil {
				return r.respond(nil, safeRemoteCode(err))
			}
			return r.respond(invocation, "")
		}
	}
	return r.respond(nil, "unknown_command")
}

func (r runner) respondBounded(data any) int {
	body, err := json.Marshal(data)
	// Leave space for the stable outer envelope in the native 1 MiB limit.
	if err != nil || len(body) > (1<<20)-128 {
		return r.respond(nil, "workspace_result_invalid")
	}
	return r.respond(json.RawMessage(body), "")
}

func safeRemoteCode(err error) string {
	var remote *workspacebridge.RemoteError
	if errors.As(err, &remote) {
		return remote.ID
	}
	return "workspace_request_failed"
}

func (r runner) workspace(dir string, args []string) int {
	if len(args) == 0 {
		return r.respond(nil, "usage_required")
	}
	if args[0] == "init" {
		var root, name string
		if _, err := parseFlags(args[1:], func(fs *flag.FlagSet) {
			fs.StringVar(&root, "root", "", "")
			fs.StringVar(&name, "name", "", "")
		}); err != nil || root == "" || name == "" {
			return r.respond(nil, "usage_required")
		}
		root, err := filepath.Abs(root)
		if err != nil {
			return r.respond(nil, "workspace_root_invalid")
		}
		config, err := workspaceconfig.Init(dir, root, name)
		if err != nil {
			return r.respond(nil, "workspace_init_failed")
		}
		return r.respond(workspaceconfig.Public(config), "")
	}
	config, err := workspaceconfig.Load(dir)
	if err != nil {
		return r.respond(nil, "workspace_config_unavailable")
	}
	var mutate func(*workspaceconfig.Config) error
	switch args[0] {
	case "status":
		if len(args) != 1 {
			return r.respond(nil, "usage_required")
		}
		return r.respond(workspaceconfig.Public(config), "")
	case "link":
		var base, id string
		if _, err := parseFlags(args[1:], func(fs *flag.FlagSet) {
			fs.StringVar(&base, "api", "", "")
			fs.StringVar(&id, "id", "", "")
		}); err != nil || id == "" {
			return r.respond(nil, "usage_required")
		}
		token, err := io.ReadAll(io.LimitReader(r.in, 4097))
		if err != nil || len(token) > 4096 {
			return r.respond(nil, "workspace_token_invalid")
		}
		connectionToken := strings.TrimSpace(string(token))
		if _, err := workspacebridge.New(base, connectionToken); err != nil {
			return r.respond(nil, "workspace_connection_invalid")
		}
		mutate = func(current *workspaceconfig.Config) error {
			current.ID, current.APIBaseURL, current.ConnectionToken = id, base, connectionToken
			return nil
		}
	case "unlink":
		if len(args) != 1 {
			return r.respond(nil, "usage_required")
		}
		mutate = func(current *workspaceconfig.Config) error {
			current.APIBaseURL, current.ConnectionToken = "", ""
			return nil
		}
	case "allow":
		// Empty list revokes all remote execution capabilities.
		capabilities := append([]string{}, args[1:]...)
		if len(capabilities) > 0 && !remoteExecutionSupported {
			return r.respond(nil, "workspace_runtime_unavailable")
		}
		mutate = func(current *workspaceconfig.Config) error {
			current.AllowedCapabilities = capabilities
			return nil
		}
		if len(capabilities) > 0 {
			_, _ = io.WriteString(r.errOut, "注意：所选 Agent/MCP 能力可能访问宿主机文件、网络和系统工具；只授权你信任的同账号环境。\n")
		}
	case "configure":
		merge := false
		if _, err := parseFlags(args[1:], func(fs *flag.FlagSet) {
			fs.BoolVar(&merge, "merge", false, "")
		}); err != nil {
			return r.respond(nil, "usage_required")
		}
		body, err := r.readJSON()
		if err != nil || strictObject(body) != nil {
			return r.respond(nil, "invalid_json")
		}
		var values struct {
			Endpoints   *[]workspaceconfig.Endpoint             `json:"endpoints"`
			Plugins     *[]string                               `json:"plugins"`
			MCPServers  *map[string]workspaceconfig.MCPServer   `json:"mcp_servers"`
			RuntimeAuth *map[string]workspaceconfig.RuntimeAuth `json:"runtime_auth"`
		}
		decoder := json.NewDecoder(strings.NewReader(string(body)))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&values) != nil {
			return r.respond(nil, "workspace_config_invalid")
		}
		// The additive desktop draft edits endpoint metadata and plugin refs
		// only; advanced maps retain explicit replacement/import semantics.
		if merge && (values.MCPServers != nil || values.RuntimeAuth != nil) {
			return r.respond(nil, "workspace_config_invalid")
		}
		var rawFields map[string]json.RawMessage
		_ = json.Unmarshal(body, &rawFields)
		var endpointFields []map[string]json.RawMessage
		if values.Endpoints != nil && json.Unmarshal(rawFields["endpoints"], &endpointFields) != nil {
			return r.respond(nil, "workspace_config_invalid")
		}
		names := map[string]bool{}
		if values.Endpoints != nil {
			for _, endpoint := range *values.Endpoints {
				if names[endpoint.Name] {
					return r.respond(nil, "workspace_config_invalid")
				}
				names[endpoint.Name] = true
			}
		}
		mutate = func(current *workspaceconfig.Config) error {
			if values.Endpoints != nil {
				next := append([]workspaceconfig.Endpoint{}, (*values.Endpoints)...)
				for i := range next {
					if _, explicit := endpointFields[i]["api_key_env"]; explicit {
						continue // Explicit empty string revokes a reference.
					}
					// Native editable fields omit credentials. Preserve only
					// the CURRENT same-name/same-URL binding under the lock;
					// never replay a pre-dialog snapshot or silently retarget
					// an existing credential to a different model endpoint.
					for _, previous := range current.Endpoints {
						if previous.Name == next[i].Name && previous.BaseURL == next[i].BaseURL {
							next[i].APIKeyEnv = previous.APIKeyEnv
							break
						}
					}
				}
				if merge {
					all := append([]workspaceconfig.Endpoint{}, current.Endpoints...)
					for _, endpoint := range next {
						replaced := false
						for i := range all {
							if all[i].Name == endpoint.Name {
								all[i], replaced = endpoint, true
								break
							}
						}
						if !replaced {
							all = append(all, endpoint)
						}
					}
					next = all
				}
				current.Endpoints = next
			}
			if values.Plugins != nil {
				if !merge {
					current.Plugins = *values.Plugins
				} else {
					for _, plugin := range *values.Plugins {
						found := false
						for _, existing := range current.Plugins {
							if existing == plugin {
								found = true
								break
							}
						}
						if !found {
							current.Plugins = append(current.Plugins, plugin)
						}
					}
				}
			}
			if values.MCPServers != nil {
				current.MCPServers = *values.MCPServers
			}
			if values.RuntimeAuth != nil {
				current.RuntimeAuth = *values.RuntimeAuth
			}
			return nil
		}
	default:
		return r.respond(nil, "unknown_command")
	}
	// Parse stdin before acquiring the config transaction. Only intended fields
	// are applied to the current snapshot; a pending configure cannot restore a
	// concurrently revoked capability or unlinked connection credential.
	config, err = workspaceconfig.Update(dir, mutate)
	if err != nil {
		return r.respond(nil, "workspace_config_invalid")
	}
	return r.respond(workspaceconfig.Public(config), "")
}

func strictObject(body json.RawMessage) error {
	trimmed := strings.TrimSpace(string(body))
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("invalid_json")
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	var visit func(int) error
	visit = func(depth int) error {
		if depth > 32 {
			return errors.New("invalid_json")
		}
		token, err := decoder.Token()
		if err != nil || token == nil {
			return errors.New("invalid_json")
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{':
				seen := map[string]bool{}
				for decoder.More() {
					token, err := decoder.Token()
					key, ok := token.(string)
					if err != nil || !ok || seen[key] {
						return errors.New("invalid_json")
					}
					seen[key] = true
					if err := visit(depth + 1); err != nil {
						return err
					}
				}
				end, err := decoder.Token()
				if err != nil || end != json.Delim('}') {
					return errors.New("invalid_json")
				}
			case '[':
				for decoder.More() {
					if err := visit(depth + 1); err != nil {
						return err
					}
				}
				end, err := decoder.Token()
				if err != nil || end != json.Delim(']') {
					return errors.New("invalid_json")
				}
			default:
				return errors.New("invalid_json")
			}
		}
		return nil
	}
	if err := visit(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("invalid_json")
	}
	return nil
}

func (r runner) connect(ctx context.Context, client *workspacebridge.Client, dir string, initial workspaceconfig.Config, once bool) error {
	current := func() (workspaceconfig.Config, error) {
		config, err := workspaceconfig.Load(dir)
		if err != nil || config.ID != initial.ID || config.APIBaseURL != initial.APIBaseURL || config.ConnectionToken != initial.ConnectionToken {
			return config, errors.New("workspace_connection_changed")
		}
		return config, nil
	}
	execute := func(ctx context.Context, capability string, args json.RawMessage) (json.RawMessage, error) {
		config, err := current()
		if err != nil {
			return nil, err
		}
		executor := workspaceruntime.Executor{Config: config, StateDir: dir}
		approvalDir, err := executor.ApprovalDirectory()
		if err != nil {
			return nil, err
		}
		executor.Approvals = workspaceruntime.FileApprovals{Dir: approvalDir}
		return executor.Execute(ctx, capability, args)
	}
	outbox, err := client.CompletionOutbox(dir)
	if err != nil {
		return err
	}
	if err := client.Heartbeat(ctx, initial.ID, initial.AllowedCapabilities); err != nil {
		return err
	}
	if err := outbox.Flush(ctx, client, initial.ID); err != nil {
		return err
	}
	if once {
		_, err := client.StepDurable(ctx, initial.ID, execute, outbox)
		return err
	}
	if err := json.NewEncoder(r.out).Encode(map[string]any{
		"ok": true, "data": map[string]string{"event": "connected"},
	}); err != nil {
		return errors.New("workspace_connection_output_failed")
	}
	_, _ = io.WriteString(r.errOut, "工作空间连接已建立；关闭进程停止接收新调用，未授权能力不会执行。\n")
	go checkLatestVersionNotice(ctx, r.errOut, version)
	connectCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	heartbeatErr := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-connectCtx.Done():
				return
			case <-ticker.C:
				config, err := current()
				if err == nil {
					err = client.Heartbeat(connectCtx, config.ID, config.AllowedCapabilities)
				}
				if err != nil {
					heartbeatErr <- err
					cancel()
					return
				}
			}
		}
	}()
	// Terminals start serving once the owner allows them, even mid-connection.
	terminals := false
	serveTerminals := func(config workspaceconfig.Config) {
		if terminals || !slices.Contains(config.AllowedCapabilities, workspaceterminal.Capability) {
			return
		}
		terminals = true
		host := &workspaceterminal.Host{Bridge: client, WorkspaceID: config.ID, Settings: func() (bool, string, error) {
			config, err := current()
			return err == nil && slices.Contains(config.AllowedCapabilities, workspaceterminal.Capability), config.Root, err
		}}
		go host.Run(connectCtx)
	}
	serveTerminals(initial)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-connectCtx.Done():
			select {
			case err := <-heartbeatErr:
				return err
			default:
				return nil
			}
		case <-ticker.C:
			config, err := current()
			if err != nil {
				return err
			}
			serveTerminals(config)
			if _, err := client.StepDurable(connectCtx, initial.ID, execute, outbox); err != nil && connectCtx.Err() == nil {
				return err
			}
		}
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit((runner{in: os.Stdin, out: os.Stdout, errOut: os.Stderr}).run(ctx, os.Args[1:]))
}
