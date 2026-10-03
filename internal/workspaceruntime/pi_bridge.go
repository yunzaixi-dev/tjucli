package workspaceruntime

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

//go:embed pi_bridge.ts
var piBridgeSource string

func (e Executor) piLinked() bool {
	return e.Config.APIBaseURL != "" && e.Config.ConnectionToken != ""
}

// Names only; neither MCP commands nor environment values enter tool schemas.
func (e Executor) piMCPReferences() map[string][]string {
	refs := map[string][]string{}
	if !slices.Contains(e.Config.AllowedCapabilities, "mcp.call") {
		return refs
	}
	for name, server := range e.Config.MCPServers {
		hosts := []string{}
		for _, host := range server.Env {
			if !slices.Contains(hosts, host) {
				hosts = append(hosts, host)
			}
		}
		slices.Sort(hosts)
		refs[name] = hosts
	}
	return refs
}

// piBridge never embeds secrets in extension source or argv. Approved MCP
// environment values are transported in a separate private ephemeral file,
// not Pi's process environment (where names such as NODE_OPTIONS are unsafe).
// The extension supplies only the selected server's refs to the fixed CLI.
func (e Executor) piBridge(state runState) (string, error) {
	executable, err := os.Executable()
	if err != nil || !filepath.IsAbs(executable) {
		return "", ErrRuntimeUnavailable
	}
	literal := func(value any) string {
		data, _ := json.Marshal(value)
		return string(data)
	}
	refs := e.piMCPReferences()
	envPath := filepath.Join(state.runDir, "pi", "mcp-env.json")
	if len(refs) != 0 {
		values := map[string]map[string]string{}
		for server, hosts := range refs {
			values[server] = map[string]string{}
			for _, host := range hosts {
				if value, ok := os.LookupEnv(host); ok {
					if len(value) > 8192 || strings.ContainsRune(value, 0) {
						return "", ErrInvalidConfig
					}
					values[server][host] = value
				}
			}
		}
		data, err := json.Marshal(values)
		if err != nil || len(data) > maxOutput || privateBytes(envPath, data) != nil {
			return "", ErrInvalidConfig
		}
	}
	source := strings.NewReplacer(
		"__TJUCLAW_EXECUTABLE__", literal(executable),
		"__TJUCLAW_CONFIG_DIR__", literal(e.StateDir),
		"__TJUCLAW_CWD__", literal(e.Config.Root),
		"__TJUCLAW_LINKED__", literal(e.piLinked()),
		"__TJUCLAW_MCP_REFS__", literal(refs),
		"__TJUCLAW_MCP_ENV_FILE__", literal(envPath),
	).Replace(piBridgeSource)
	path := filepath.Join(state.runDir, "pi", "workspace-bridge.ts")
	if privateBytes(path, []byte(source)) != nil {
		return "", ErrInvalidConfig
	}
	return path, nil
}
