// Package workspaceconfig stores the identity and private configuration of one
// system environment. A project directory is not an environment identity.
package workspaceconfig

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

const (
	Version        = 1
	ConfigFileName = "config.json"
	StateDirName   = "state"
	MaxConfigBytes = 1 << 20
)

type Config struct {
	Version             int                    `json:"version"`
	ID                  string                 `json:"id"`
	Name                string                 `json:"name"`
	Root                string                 `json:"root"`
	APIBaseURL          string                 `json:"api_base_url,omitempty"`
	ConnectionToken     string                 `json:"connection_token,omitempty"`
	AllowedCapabilities []string               `json:"allowed_capabilities"`
	Endpoints           []Endpoint             `json:"endpoints"`
	Plugins             []string               `json:"plugins"`
	MCPServers          map[string]MCPServer   `json:"mcp_servers"`
	RuntimeAuth         map[string]RuntimeAuth `json:"runtime_auth,omitempty"`
}

// RuntimeAuth attaches only credentials explicitly selected by the local owner.
// Source is an absolute credential-file path for host-file, or a host variable
// NAME for oauth-env; never a token, HOME directory or discovery expression.
// Absence means independent credentials, not permission to inherit host auth.
// The runtime resolves the source privately and must fail closed when unusable.
// Durable sessions remain in workspace-owned state, not in this attachment.
type RuntimeAuth struct {
	Mode   string `json:"mode"`
	Source string `json:"source"`
	Model  string `json:"model,omitempty"`
}

// Endpoint URLs are exclusively for execution on this environment's host.
// Callers select endpoints by Name, never by a caller-supplied URL. This package
// performs no network requests and does not authorize cloud-side URL fetching.
type Endpoint struct {
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	Model     string `json:"model"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
}

type MCPServer struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	// Env maps child variable names to host variable names, NOT literal values
	// or interpolation expressions. Only explicitly referenced values are read.
	Env map[string]string `json:"env,omitempty"`
}

var (
	// Server-issued base64url identities may start with '-' or '_'; this
	// deliberately does not change endpoint, MCP server or executable names.
	identityPattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]{0,127}$`)
	namePattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	envPattern      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

// Init creates a new identity, empty opt-in configuration, and private state.
// It never replaces an existing configuration, including concurrent Init calls.
func Init(dir, root, name string) (Config, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return Config{}, errors.New("workspace identity generation failed")
	}
	c := Config{
		Version: Version, ID: hex.EncodeToString(id), Name: name, Root: root,
		AllowedCapabilities: []string{}, Endpoints: []Endpoint{},
		Plugins: []string{}, MCPServers: map[string]MCPServer{},
	}
	if err := Validate(c); err != nil {
		return Config{}, err
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return Config{}, errors.New("workspace root must be an existing directory")
	}
	if err := writeConfig(dir, c, true); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Load rejects unknown fields, oversized documents, insecure permissions and
// links. It never creates or repairs a missing configuration.
func Load(dir string) (Config, error) {
	r, err := openPrivateDir(dir, false)
	if err != nil {
		return Config{}, err
	}
	defer r.Close()
	return loadConfig(r)
}

// loadConfig reads through an already anchored directory. Writers call it only
// after acquiring the directory's cross-process lock.
func loadConfig(r *os.Root) (Config, error) {
	f, err := openRegular(r, ConfigFileName)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxConfigBytes+1))
	if err != nil || len(data) > MaxConfigBytes {
		return Config{}, errors.New("workspace config read failed or exceeds limit")
	}
	var c Config
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		// Decoder errors can quote secret-containing input. Do not surface them.
		return Config{}, errors.New("invalid workspace config JSON")
	}
	if d.Decode(new(any)) != io.EOF {
		return Config{}, errors.New("workspace config must contain one JSON object")
	}
	if err := Validate(c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Save validates before touching disk and atomically replaces the whole config
// under the same cross-process lock as Init and Update. For changes to a current
// configuration use Update: Save cannot identify a caller's stale snapshot.
func Save(dir string, c Config) error {
	return writeConfig(dir, c, false)
}

// Update reads the current configuration, invokes mutate, validates and
// atomically publishes it while holding a private cross-process lock.
// mutate must only perform short in-memory changes: parse stdin before calling
// Update, and never call Save, Init or Update recursively for the same dir.
// A callback or validation error leaves the existing configuration untouched.
func Update(dir string, mutate func(*Config) error) (Config, error) {
	if mutate == nil {
		return Config{}, errors.New("workspace config update requires a callback")
	}
	var updated Config
	err := withConfigLock(dir, false, func(r *os.Root) error {
		current, err := loadConfig(r)
		if err != nil {
			return err
		}
		if err := mutate(&current); err != nil {
			return err
		}
		if err := publishConfig(r, current, false); err != nil {
			return err
		}
		updated = current
		return nil
	})
	if err != nil {
		return Config{}, err
	}
	return updated, nil
}

func Validate(c Config) error {
	if c.Version != Version {
		return errors.New("unsupported workspace config version")
	}
	if !identityPattern.MatchString(c.ID) {
		return errors.New("invalid workspace identity")
	}
	if strings.TrimSpace(c.Name) == "" || len(c.Name) > 256 || hasControl(c.Name) {
		return errors.New("invalid workspace name")
	}
	if !absoluteClean(c.Root) {
		return errors.New("workspace root must be a clean absolute path")
	}
	if c.APIBaseURL != "" {
		if err := validateURL(c.APIBaseURL, false); err != nil {
			return errors.New("invalid workspace API base URL")
		}
	}
	if len(c.ConnectionToken) > 8192 || hasControl(c.ConnectionToken) || strings.ContainsAny(c.ConnectionToken, " \t") {
		return errors.New("invalid workspace connection token")
	}
	if c.ConnectionToken != "" && c.APIBaseURL == "" {
		return errors.New("workspace connection token requires an API base URL")
	}
	caps := map[string]bool{}
	for _, capability := range c.AllowedCapabilities {
		switch capability {
		case "pi.prompt", "claude.prompt", "codex.prompt", "mcp.call", "terminal.open":
		default:
			return errors.New("unsupported workspace capability")
		}
		if caps[capability] {
			return errors.New("duplicate workspace capability")
		}
		caps[capability] = true
	}
	if len(c.Endpoints) > 128 || len(c.Plugins) > 128 || len(c.MCPServers) > 128 {
		return errors.New("workspace configuration collection limit exceeded")
	}
	names := map[string]bool{}
	for _, endpoint := range c.Endpoints {
		if !namePattern.MatchString(endpoint.Name) || names[endpoint.Name] {
			return errors.New("invalid or duplicate workspace endpoint name")
		}
		names[endpoint.Name] = true
		if validateURL(endpoint.BaseURL, true) != nil {
			return errors.New("invalid workspace endpoint URL")
		}
		if strings.TrimSpace(endpoint.Model) == "" || len(endpoint.Model) > 256 || hasControl(endpoint.Model) {
			return errors.New("invalid workspace endpoint model")
		}
		if endpoint.APIKeyEnv != "" && !envPattern.MatchString(endpoint.APIKeyEnv) {
			return errors.New("endpoint API key must reference an environment variable name")
		}
	}
	plugins := map[string]bool{}
	for _, plugin := range c.Plugins {
		// Plugins are declarative references to existing paths or packages. No
		// package manager, installation, expansion or shell is invoked here.
		if strings.TrimSpace(plugin) != plugin || plugin == "" || len(plugin) > 4096 || hasControl(plugin) || plugins[plugin] {
			return errors.New("invalid or duplicate workspace plugin reference")
		}
		plugins[plugin] = true
	}
	for name, server := range c.MCPServers {
		if !namePattern.MatchString(name) {
			return errors.New("invalid workspace MCP server name")
		}
		if err := server.Validate(); err != nil {
			return err
		}
	}
	for runtime, auth := range c.RuntimeAuth {
		switch runtime {
		case "pi", "claude", "codex":
		default:
			return errors.New("unsupported workspace auth runtime")
		}
		if err := auth.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks the declarative attachment without reading credentials or
// probing the host. File safety and auth-format checks belong to the runtime
// when it opens the owner-selected file, not to config publication.
func (a RuntimeAuth) Validate() error {
	switch a.Mode {
	case "host-file":
		if len(a.Source) > 4096 || !absoluteClean(a.Source) || filepath.Dir(a.Source) == a.Source {
			return errors.New("workspace auth source must be an absolute credential file path")
		}
	case "oauth-env":
		if !envPattern.MatchString(a.Source) {
			return errors.New("workspace auth source must reference an environment variable name")
		}
	default:
		return errors.New("unsupported workspace auth attachment mode")
	}
	if a.Model != "" && (strings.TrimSpace(a.Model) != a.Model || len(a.Model) > 256 || hasControl(a.Model)) {
		return errors.New("invalid workspace auth model")
	}
	return nil
}

// Validate is also used by the stdio helper before it starts a process.
func (s MCPServer) Validate() error {
	if s.Command == "" || strings.TrimSpace(s.Command) != s.Command || len(s.Command) > 4096 || hasControl(s.Command) {
		return errors.New("invalid MCP executable")
	}
	if strings.ContainsAny(s.Command, `/\`) {
		if !absoluteClean(s.Command) {
			return errors.New("MCP executable path must be absolute")
		}
	} else if !namePattern.MatchString(s.Command) {
		return errors.New("MCP command must name one executable, not a shell expression")
	}
	if len(s.Args) > 128 || len(s.Env) > 128 {
		return errors.New("MCP configuration limit exceeded")
	}
	size := len(s.Command)
	for _, arg := range s.Args {
		if len(arg) > 8192 || hasControl(arg) {
			return errors.New("invalid MCP argument")
		}
		size += len(arg)
	}
	if size > 64<<10 {
		return errors.New("MCP arguments exceed limit")
	}
	for child, host := range s.Env {
		if !envPattern.MatchString(child) || !envPattern.MatchString(host) {
			return errors.New("MCP environment must reference variable names, not literal values")
		}
	}
	return nil
}

// Public returns a detached view. Token, Env values, auth sources and MCP argv are absent;
// argv may contain explicitly configured literal secrets. Editing this result
// cannot modify the private config. APIKeyEnv is a name, not a secret.
func Public(c Config) any {
	type publicServer struct {
		Command string   `json:"command"`
		Env     []string `json:"env,omitempty"`
	}
	type publicAuth struct {
		Mode       string `json:"mode"`
		Model      string `json:"model,omitempty"`
		Configured bool   `json:"configured"`
	}
	auth := make(map[string]publicAuth, len(c.RuntimeAuth))
	for name, attachment := range c.RuntimeAuth {
		auth[name] = publicAuth{attachment.Mode, attachment.Model, true}
	}
	servers := make(map[string]publicServer, len(c.MCPServers))
	for name, server := range c.MCPServers {
		env := make([]string, 0, len(server.Env))
		for key := range server.Env {
			env = append(env, key)
		}
		// Stable output is useful for status consumers.
		slices.Sort(env)
		servers[name] = publicServer{server.Command, env}
	}
	return struct {
		Version             int                     `json:"version"`
		ID                  string                  `json:"id"`
		Name                string                  `json:"name"`
		Root                string                  `json:"root"`
		APIBaseURL          string                  `json:"api_base_url,omitempty"`
		Linked              bool                    `json:"linked"`
		AllowedCapabilities []string                `json:"allowed_capabilities"`
		Endpoints           []Endpoint              `json:"endpoints"`
		Plugins             []string                `json:"plugins"`
		MCPServers          map[string]publicServer `json:"mcp_servers"`
		RuntimeAuth         map[string]publicAuth   `json:"runtime_auth,omitempty"`
	}{
		c.Version, c.ID, c.Name, c.Root, c.APIBaseURL, c.APIBaseURL != "" && c.ConnectionToken != "",
		append([]string{}, c.AllowedCapabilities...), append([]Endpoint{}, c.Endpoints...),
		append([]string{}, c.Plugins...), servers, auth,
	}
}

func absoluteClean(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !hasControl(path)
}

func hasControl(value string) bool {
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}

func validateURL(raw string, localEndpoint bool) error {
	bad := errors.New("unsafe URL")
	if len(raw) > 4096 || strings.TrimSpace(raw) != raw || hasControl(raw) || strings.ContainsAny(raw, `\ `) {
		return bad
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return bad
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return bad
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.ContainsAny(host, "%/\\") || strings.HasSuffix(u.Host, ":") {
		return bad
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return bad
		}
	}
	local := host == "localhost" || strings.HasSuffix(host, ".localhost")
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return bad
		}
		local = ip.IsLoopback() || (localEndpoint && ip.IsPrivate())
	} else {
		// Reject ambiguous numeric hosts and malformed DNS labels without DNS
		// lookups. Validation must not contact the network.
		onlyNumeric := true
		for _, label := range strings.Split(host, ".") {
			if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return bad
			}
			for _, ch := range label {
				if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
					return bad
				}
				if ch < '0' || ch > '9' {
					onlyNumeric = false
				}
			}
		}
		if onlyNumeric || len(host) > 253 || strings.HasPrefix(host, "0x") {
			return bad
		}
	}
	if u.Scheme == "http" && !local {
		return bad
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." || hasControl(segment) || strings.Contains(segment, `\`) {
			return bad
		}
	}
	return nil
}
