package workspaceruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yunzaixi-dev/tjucli/internal/workspaceconfig"
)

type ApprovalDecision string

const (
	ApprovalAllow ApprovalDecision = "allow"
	ApprovalDeny  ApprovalDecision = "deny"
	approvalWait                   = 30 * time.Second
	// Reserve space for the CLI/native JSON envelope, not just the array.
	maxPendingApprovalBytes = (1 << 20) - (4 << 10)
)

type ApprovalRequest struct {
	ID        string          `json:"id"`
	Runtime   string          `json:"runtime"`
	SessionID string          `json:"session_id"`
	Kind      string          `json:"kind"`
	Tool      string          `json:"tool,omitempty"`
	Input     json.RawMessage `json:"input"`
	ExpiresAt time.Time       `json:"expires_at"`
}

type ApprovalResponse struct {
	RequestID string           `json:"request_id"`
	Decision  ApprovalDecision `json:"decision"`
}

// ApprovalHandler is LOCAL owner transport. Never implement it using a remote
// prompt argument, model answer, same-account invocation result or auto-policy.
// Implementations must honor ctx and must not persist broad approval rules.
type ApprovalHandler interface {
	RequestApproval(context.Context, ApprovalRequest) (ApprovalDecision, error)
}

type ApprovalFunc func(context.Context, ApprovalRequest) (ApprovalDecision, error)

func (f ApprovalFunc) RequestApproval(ctx context.Context, request ApprovalRequest) (ApprovalDecision, error) {
	return f(ctx, request)
}

func approvalResponse(raw []byte, id string) (ApprovalDecision, error) {
	var response ApprovalResponse
	if decodeObject(raw, &response, "request_id", "decision") != nil ||
		response.RequestID != id || (response.Decision != ApprovalAllow && response.Decision != ApprovalDeny) {
		return ApprovalDeny, ErrApprovalRequired
	}
	return response.Decision, nil
}

func (e Executor) approve(ctx context.Context, state runState, runtime, kind, tool string, input json.RawMessage) bool {
	if e.Approvals == nil || state.session == nil || len(input) > maxPrompt || !json.Valid(input) {
		return false
	}
	// Sanitize for the local display only. An allow reply cannot alter the
	// native request/input and never updates persistent/session permission rules.
	secrets := append([]string{}, state.secrets...)
	for _, entry := range state.env {
		if value, ok := strings.CutPrefix(entry, keyVariable+"="); ok && value != "" {
			secrets = append(secrets, value)
		}
	}
	for _, server := range e.Config.MCPServers {
		for _, host := range server.Env {
			if value := os.Getenv(host); value != "" {
				secrets = append(secrets, value)
			}
		}
	}
	var data any
	if json.Unmarshal(input, &data) != nil {
		return false
	}
	var clean func(any) any
	clean = func(value any) any {
		switch v := value.(type) {
		case string:
			for _, secret := range secrets {
				v = strings.ReplaceAll(v, secret, "[redacted]")
			}
			for _, endpoint := range e.Config.Endpoints {
				if endpoint.BaseURL != "" {
					v = strings.ReplaceAll(v, endpoint.BaseURL, "[endpoint]")
				}
			}
			return v
		case []any:
			for i := range v {
				v[i] = clean(v[i])
			}
		case map[string]any:
			for k := range v {
				v[k] = clean(v[k])
			}
		}
		return value
	}
	input, _ = json.Marshal(clean(data))
	id, err := randomID()
	if err != nil {
		return false
	}
	waitCtx, cancel := context.WithTimeout(ctx, approvalWait)
	defer cancel()
	deadline, _ := waitCtx.Deadline()
	request := ApprovalRequest{ID: id, Runtime: runtime, SessionID: state.session.ID,
		Kind: kind, Tool: tool, Input: input, ExpiresAt: deadline.UTC()}
	type outcome struct {
		decision ApprovalDecision
		err      error
	}
	reply := make(chan outcome, 1)
	go func() {
		decision, err := e.Approvals.RequestApproval(waitCtx, request)
		reply <- outcome{decision, err}
	}()
	select {
	case <-waitCtx.Done():
		return false
	case result := <-reply:
		return waitCtx.Err() == nil && result.err == nil && result.decision == ApprovalAllow
	}
}

// FileApprovals uses <Dir>/<random request ID>/{request,response}.json, 0700/0600.
// A native owner UI writes response.json atomically. The ID must match exactly.
// Responses are one-shot; timeout/cancel removes the request directory.
type FileApprovals struct {
	Dir     string
	Timeout time.Duration
}

func (e Executor) ApprovalDirectory() (string, error) {
	canonical, err := filepath.EvalSymlinks(e.StateDir)
	if !filepath.IsAbs(e.StateDir) || err != nil || canonical != filepath.Clean(e.StateDir) {
		return "", ErrInvalidConfig
	}
	base := filepath.Join(e.StateDir, "runtime")
	if privateDir(base) != nil {
		return "", ErrInvalidConfig
	}
	dir := filepath.Join(base, "approvals")
	if privateDir(dir) != nil {
		return "", ErrInvalidConfig
	}
	return dir, nil
}

func (e Executor) readApproval(ctx context.Context, dir, id string) (ApprovalRequest, error) {
	var request ApprovalRequest
	info, err := os.Lstat(filepath.Join(dir, id))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
		workspaceconfig.CheckPrivateDir(filepath.Join(dir, id)) != nil {
		return request, ErrApprovalRequired
	}
	raw, err := readPrivateFile(filepath.Join(dir, id, "request.json"), maxArguments)
	if err != nil || json.Unmarshal(raw, &request) != nil || request.ID != id ||
		!request.ExpiresAt.After(time.Now()) || request.ExpiresAt.After(time.Now().Add(approvalWait+time.Second)) {
		return request, ErrApprovalRequired
	}
	session, err := e.Session(ctx, request.SessionID)
	if err != nil || session.Runtime != request.Runtime {
		return request, ErrApprovalRequired
	}
	return request, nil
}

func (e Executor) PendingApprovals(ctx context.Context) ([]ApprovalRequest, error) {
	if ctx == nil {
		return nil, ErrInvalidArguments
	}
	dir, err := e.ApprovalDirectory()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) > maxSessions {
		return nil, ErrApprovalRequired
	}
	result := []ApprovalRequest{}
	size := 2 // JSON array brackets
	for _, entry := range entries {
		if ctx.Err() != nil {
			return nil, ErrCanceled
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !sessionIDPattern.MatchString(entry.Name()) {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, entry.Name(), "response.json")); err == nil {
			continue
		}
		if request, err := e.readApproval(ctx, dir, entry.Name()); err == nil {
			encoded, err := json.Marshal(request)
			if err != nil {
				return nil, ErrApprovalRequired
			}
			added := len(encoded)
			if len(result) != 0 {
				added++ // comma
			}
			if size+added > maxPendingApprovalBytes {
				// Never silently hide a partial queue from the native owner.
				return nil, ErrOutputLimit
			}
			size += added
			result = append(result, request)
		}
	}
	return result, nil
}

// RespondApproval is local-owner-only. Parent must never offer this command
// through the Pi model bridge, connector capability list or remote caller API.
func (e Executor) RespondApproval(ctx context.Context, id string, decision ApprovalDecision) error {
	if ctx == nil || !sessionIDPattern.MatchString(id) || (decision != ApprovalAllow && decision != ApprovalDeny) {
		return ErrInvalidArguments
	}
	dir, err := e.ApprovalDirectory()
	if err != nil {
		return err
	}
	if _, err := e.readApproval(ctx, dir, id); err != nil {
		return err
	}
	unlock, err := lockSession(ctx, filepath.Join(dir, id, ".reply.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	path := filepath.Join(dir, id, "response.json")
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return ErrApprovalRequired
	}
	return atomicPrivateJSON(path, ApprovalResponse{RequestID: id, Decision: decision})
}

func approvalContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 || timeout > approvalWait {
		timeout = approvalWait
	}
	return context.WithTimeout(ctx, timeout)
}

func (f FileApprovals) RequestApproval(ctx context.Context, request ApprovalRequest) (ApprovalDecision, error) {
	if !sessionIDPattern.MatchString(request.ID) || !filepath.IsAbs(f.Dir) {
		return ApprovalDeny, ErrInvalidConfig
	}
	canonical, err := filepath.EvalSymlinks(f.Dir)
	if err != nil || canonical != filepath.Clean(f.Dir) || privateDir(f.Dir) != nil {
		return ApprovalDeny, ErrInvalidConfig
	}
	ctx, cancel := approvalContext(ctx, f.Timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	request.ExpiresAt = deadline.UTC()
	dir := filepath.Join(f.Dir, request.ID)
	if os.Mkdir(dir, 0700) != nil {
		return ApprovalDeny, ErrInvalidConfig
	}
	defer os.RemoveAll(dir)
	if privateDir(dir) != nil {
		return ApprovalDeny, ErrInvalidConfig
	}
	if atomicPrivateJSON(filepath.Join(dir, "request.json"), request) != nil {
		return ApprovalDeny, ErrInvalidConfig
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return ApprovalDeny, ErrApprovalRequired
		}
		path := filepath.Join(dir, "response.json")
		if _, err := os.Lstat(path); err == nil {
			raw, err := readPrivateFile(path, maxPrompt)
			if err != nil {
				return ApprovalDeny, ErrApprovalRequired
			}
			return approvalResponse(raw, request.ID)
		} else if !errors.Is(err, os.ErrNotExist) {
			return ApprovalDeny, ErrApprovalRequired
		}
		select {
		case <-ctx.Done():
			return ApprovalDeny, ErrApprovalRequired
		case <-ticker.C:
		}
	}
}

// StdioApprovals exchanges newline JSON ApprovalRequest/ApprovalResponse on a
// dedicated LOCAL bidirectional stream (not model stdin). Cancel/timeout closes
// the stream to interrupt blocked writes/reads; the caller must reconnect.
type StdioApprovals struct {
	Stream  io.ReadWriteCloser
	Timeout time.Duration
	mu      sync.Mutex
	scanner *bufio.Scanner
	gate    chan struct{}
}

func (s *StdioApprovals) RequestApproval(ctx context.Context, request ApprovalRequest) (ApprovalDecision, error) {
	if s.Stream == nil {
		return ApprovalDeny, ErrInvalidConfig
	}
	ctx, cancel := approvalContext(ctx, s.Timeout)
	defer cancel()
	s.mu.Lock()
	if s.gate == nil {
		s.gate = make(chan struct{}, 1)
		s.scanner = bufio.NewScanner(s.Stream)
		s.scanner.Buffer(make([]byte, 4096), maxPrompt)
	}
	gate, scanner := s.gate, s.scanner
	s.mu.Unlock()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return ApprovalDeny, ErrApprovalRequired
	}
	deadline, _ := ctx.Deadline()
	request.ExpiresAt = deadline.UTC()
	reply := make(chan []byte, 1)
	go func() {
		if json.NewEncoder(s.Stream).Encode(request) != nil || !scanner.Scan() {
			reply <- nil
			return
		}
		reply <- append([]byte(nil), scanner.Bytes()...)
	}()
	select {
	case <-ctx.Done():
		_ = s.Stream.Close()
		<-reply
		return ApprovalDeny, ErrApprovalRequired
	case raw := <-reply:
		if ctx.Err() != nil {
			return ApprovalDeny, ErrApprovalRequired
		}
		return approvalResponse(raw, request.ID)
	}
}
