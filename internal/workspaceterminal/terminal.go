// Package workspaceterminal runs the live terminals the owner opens on this
// computer from TJUClaw. It only acts when the local owner allowed
// "terminal.open", only starts shells inside the configured root, and holds
// no state beyond the running shells: the API relays bytes, nothing more.
package workspaceterminal

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yunzaixi-dev/tjucli/internal/workspacebridge"
)

const (
	Capability   = "terminal.open"
	maxTerminals = 4
	chunk        = 24 << 10 // the relay's frame limit
)

// Bridge is the part of the connector client the terminals use.
type Bridge interface {
	RelayPoll(ctx context.Context, workspaceID string) ([]workspacebridge.RelayEvent, error)
	TerminalOutput(ctx context.Context, workspaceID, terminalID string, data []byte, state string, exitCode *int, failure string) error
}

// Process is a shell attached to a pseudo-terminal (or, where there is
// none, to pipes).
type Process interface {
	io.ReadWriter
	Resize(cols, rows int) error
	Wait() (int, error)
	Kill()
}

// Host serves terminal events for one connected computer.
type Host struct {
	Bridge      Bridge
	WorkspaceID string
	// Settings rereads the local configuration: whether terminals are
	// allowed and the root they must stay in. Rechecked on every open.
	Settings func() (allowed bool, root string, err error)
	// Start is replaceable in tests.
	Start func(dir string, cols, rows int) (Process, error)

	mu    sync.Mutex
	terms map[string]Process
}

// Run serves until ctx ends. Relay errors (offline API, terminal not yet
// reported by heartbeat) are retried after a pause.
func (h *Host) Run(ctx context.Context) {
	if h.Start == nil {
		h.Start = startShell
	}
	defer h.closeAll()
	for ctx.Err() == nil {
		events, err := h.Bridge.RelayPoll(ctx, h.WorkspaceID)
		if err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(3 * time.Second):
			}
			continue
		}
		for _, event := range events {
			h.handle(ctx, event)
		}
	}
}

func (h *Host) get(id string) Process {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.terms[id]
}

func (h *Host) handle(ctx context.Context, event workspacebridge.RelayEvent) {
	switch event.Type {
	case "open":
		h.open(ctx, event)
	case "input":
		if p := h.get(event.Terminal); p != nil {
			if data, err := base64.StdEncoding.DecodeString(event.Data); err == nil {
				_, _ = p.Write(data)
			}
		}
	case "resize":
		if p := h.get(event.Terminal); p != nil && event.Cols > 0 && event.Rows > 0 {
			_ = p.Resize(event.Cols, event.Rows)
		}
	case "close":
		if p := h.get(event.Terminal); p != nil {
			p.Kill()
		}
	}
}

func (h *Host) refuse(ctx context.Context, terminal, failure string) {
	_ = h.Bridge.TerminalOutput(ctx, h.WorkspaceID, terminal, nil, "closed", nil, failure)
}

func (h *Host) open(ctx context.Context, event workspacebridge.RelayEvent) {
	if h.get(event.Terminal) != nil {
		return
	}
	allowed, root, err := h.Settings()
	if err != nil || !allowed {
		h.refuse(ctx, event.Terminal, "terminal_not_allowed")
		return
	}
	dir, ok := Inside(root, event.Cwd)
	if !ok {
		h.refuse(ctx, event.Terminal, "terminal_cwd_outside_root")
		return
	}
	h.mu.Lock()
	if h.terms == nil {
		h.terms = map[string]Process{}
	}
	full := len(h.terms) >= maxTerminals
	h.mu.Unlock()
	if full {
		h.refuse(ctx, event.Terminal, "terminal_limit_reached")
		return
	}
	p, err := h.Start(dir, event.Cols, event.Rows)
	if err != nil {
		h.refuse(ctx, event.Terminal, "terminal_start_failed")
		return
	}
	h.mu.Lock()
	h.terms[event.Terminal] = p
	h.mu.Unlock()
	_ = h.Bridge.TerminalOutput(ctx, h.WorkspaceID, event.Terminal, nil, "open", nil, "")
	go h.pump(ctx, event.Terminal, p)
}

// pump sends the shell's output in order until it exits, then its code.
func (h *Host) pump(ctx context.Context, id string, p Process) {
	buf := make([]byte, chunk)
	for {
		n, err := p.Read(buf)
		if n > 0 {
			if sendErr := h.Bridge.TerminalOutput(ctx, h.WorkspaceID, id, buf[:n], "", nil, ""); sendErr != nil {
				var remote *workspacebridge.RemoteError
				if errors.As(sendErr, &remote) || ctx.Err() != nil {
					// The browser side is gone; stop the shell.
					p.Kill()
				}
			}
		}
		if err != nil {
			break
		}
	}
	code, _ := p.Wait()
	h.mu.Lock()
	delete(h.terms, id)
	h.mu.Unlock()
	if ctx.Err() == nil {
		_ = h.Bridge.TerminalOutput(ctx, h.WorkspaceID, id, nil, "closed", &code, "")
	}
}

func (h *Host) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.terms {
		p.Kill()
	}
}

// Inside resolves the requested directory (the root when empty) and reports
// whether it, with symlinks followed, lies within root.
func Inside(root, requested string) (string, bool) {
	if root == "" || !filepath.IsAbs(root) {
		return "", false
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	dir := root
	if requested != "" {
		dir = filepath.FromSlash(requested)
		if !filepath.IsAbs(dir) {
			return "", false
		}
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", false
	}
	if info, err := os.Stat(realDir); err != nil || !info.IsDir() {
		return "", false
	}
	rel, err := filepath.Rel(realRoot, realDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return realDir, true
}

// shellEnv is the connector's environment with a terminal type the browser
// terminal understands.
func shellEnv() []string {
	env := []string{}
	for _, item := range os.Environ() {
		if strings.HasPrefix(item, "TERM=") || strings.HasPrefix(item, "COLORTERM=") {
			continue
		}
		env = append(env, item)
	}
	return append(env, "TERM=xterm-256color", "COLORTERM=truecolor", "TJUCLAW_TERMINAL=1")
}
