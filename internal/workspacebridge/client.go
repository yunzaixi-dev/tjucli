// Package workspacebridge implements the outbound-only workspace connector.
package workspacebridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	maxResponse    = 2 << 20
	maxArguments   = 16 << 10
	maxResultBytes = 32 << 10
)

type Workspace struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Kind         string     `json:"kind"`
	Capabilities []string   `json:"capabilities"`
	Online       bool       `json:"online"`
	LastSeenAt   *time.Time `json:"last_seen_at"`
	CreatedAt    time.Time  `json:"created_at"`
}

type Invocation struct {
	ID                string          `json:"id"`
	SourceWorkspaceID string          `json:"source_workspace_id,omitempty"`
	TargetWorkspaceID string          `json:"target_workspace_id"`
	Capability        string          `json:"capability"`
	Arguments         json.RawMessage `json:"arguments"`
	Status            string          `json:"status"`
	Result            json.RawMessage `json:"result,omitempty"`
	Error             *RemoteError    `json:"error,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
	TimeoutSeconds    int             `json:"timeout_seconds,omitempty"`
	DeadlineAt        *time.Time      `json:"deadline_at,omitempty"`
}

// RemoteError deliberately excludes upstream messages, URLs and credentials.
type RemoteError struct {
	ID string `json:"id"`
}

func (e *RemoteError) Error() string { return e.ID }

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func New(baseURL, token string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u == nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"))) ||
		(u.Path != "" && u.Path != "/" && u.Path != "/api" && u.Path != "/api/") ||
		token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("invalid_workspace_connection")
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"), token: token,
		http: &http.Client{Timeout: 35 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}, nil
}

func validID(id string) bool {
	if len(id) < 1 || len(id) > 128 {
		return false
	}
	for _, ch := range id {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
			return false
		}
	}
	return true
}

func (c *Client) request(ctx context.Context, method, path string, body any, output any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil || len(data) > 1<<20 {
			return errors.New("workspace_request_invalid")
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return errors.New("workspace_request_invalid")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("workspace_connection_unavailable")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(data) > maxResponse {
		return errors.New("workspace_response_invalid")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Only accept a stable machine code, never echo arbitrary response text.
		var envelope struct {
			Error RemoteError `json:"error"`
		}
		if json.Unmarshal(data, &envelope) == nil && knownErrorID(envelope.Error.ID) {
			return &envelope.Error
		}
		return errors.New("workspace_request_failed")
	}
	if output != nil && json.Unmarshal(data, output) != nil {
		return errors.New("workspace_response_invalid")
	}
	return nil
}

func knownErrorID(id string) bool {
	switch id {
	case "workspace_not_found", "workspace_invocation_not_found", "workspace_token_required",
		"invalid_workspace_request", "workspace_capability_denied", "workspace_offline",
		"workspace_limit_reached", "workspace_invocation_conflict", "workspace_storage_unavailable",
		"auth_not_configured", "session_required", "origin_rejected", "auth_unavailable",
		"json_required", "request_too_large":
		return true
	default:
		return false
	}
}

func (c *Client) Heartbeat(ctx context.Context, id string, capabilities []string) error {
	if !validID(id) {
		return errors.New("workspace_id_invalid")
	}
	if capabilities == nil {
		capabilities = []string{}
	}
	return c.request(ctx, http.MethodPost, "/workspaces/"+id+"/heartbeat", map[string]any{"capabilities": capabilities}, nil)
}

func (c *Client) Poll(ctx context.Context, id string) (*Invocation, error) {
	if !validID(id) {
		return nil, errors.New("workspace_id_invalid")
	}
	var result struct {
		Invocation *Invocation `json:"invocation"`
	}
	err := c.request(ctx, http.MethodPost, "/workspaces/"+id+"/poll", struct{}{}, &result)
	return result.Invocation, err
}

func (c *Client) Complete(ctx context.Context, workspaceID, invocationID string, result json.RawMessage, executionErr error) error {
	if !validID(workspaceID) || !validID(invocationID) {
		return errors.New("workspace_id_invalid")
	}
	var payload any
	if executionErr != nil {
		// Executor errors can contain process-specific details; never forward them.
		payload = map[string]any{"error": RemoteError{ID: "workspace_execution_failed"}}
	} else {
		if !json.Valid(result) || len(result) > maxResultBytes {
			return errors.New("workspace_result_invalid")
		}
		payload = map[string]any{"result": result}
	}
	return c.request(ctx, http.MethodPost, "/workspaces/"+workspaceID+"/invocations/"+invocationID+"/complete", payload, nil)
}

func (c *Client) Invoke(ctx context.Context, target, capability string, args json.RawMessage) (*Invocation, error) {
	return c.InvokeWithTimeout(ctx, target, capability, args, 0)
}

// InvokeWithTimeout selects a bounded server execution deadline, not a process
// policy override. The target still enforces its local execution limit.
func (c *Client) InvokeWithTimeout(ctx context.Context, target, capability string, args json.RawMessage, timeoutSeconds int) (*Invocation, error) {
	if !validID(target) || !json.Valid(args) || len(args) > maxArguments || timeoutSeconds < 0 || timeoutSeconds > 86400 {
		return nil, errors.New("workspace_invocation_invalid")
	}
	var result struct {
		Invocation Invocation `json:"invocation"`
	}
	payload := map[string]any{"capability": capability, "arguments": args}
	if timeoutSeconds != 0 {
		payload["timeout_seconds"] = timeoutSeconds
	}
	err := c.request(ctx, http.MethodPost, "/workspaces/"+target+"/invocations", payload, &result)
	return &result.Invocation, err
}

func (c *Client) Get(ctx context.Context, id string) (*Invocation, error) {
	if !validID(id) {
		return nil, errors.New("workspace_id_invalid")
	}
	var result struct {
		Invocation Invocation `json:"invocation"`
	}
	err := c.request(ctx, http.MethodGet, "/workspace-invocations/"+id, nil, &result)
	return &result.Invocation, err
}

func (c *Client) List(ctx context.Context) ([]Workspace, error) {
	var result struct {
		Workspaces []Workspace `json:"workspaces"`
	}
	err := c.request(ctx, http.MethodGet, "/workspaces", nil, &result)
	return result.Workspaces, err
}
