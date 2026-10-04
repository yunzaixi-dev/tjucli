package workspaceterminal

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yunzaixi-dev/tjucli/internal/workspacebridge"
)

func TestInsideKeepsShellsWithinTheRoot(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	outside := t.TempDir()
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	realRoot, _ := filepath.EvalSymlinks(root)
	for _, c := range []struct {
		requested string
		ok        bool
	}{
		{"", true}, {project, true}, {root, true},
		{outside, false}, {link, false}, {"relative/path", false},
		{filepath.Join(root, "missing"), false}, {filepath.Join(project, "..", ".."), false},
	} {
		dir, ok := Inside(root, c.requested)
		if ok != c.ok {
			t.Fatalf("Inside(%q) = %v, want %v", c.requested, ok, c.ok)
		}
		if ok && !strings.HasPrefix(dir, realRoot) {
			t.Fatalf("Inside(%q) resolved to %q", c.requested, dir)
		}
	}
	if _, ok := Inside("", ""); ok {
		t.Fatal("no root accepted")
	}
}

type output struct {
	terminal, state, failure string
	data                     []byte
	exit                     *int
}

type fakeBridge struct {
	mu      sync.Mutex
	events  chan []workspacebridge.RelayEvent
	outputs []output
	changed chan struct{}
}

func newFakeBridge() *fakeBridge {
	return &fakeBridge{events: make(chan []workspacebridge.RelayEvent, 8), changed: make(chan struct{}, 1024)}
}

func (b *fakeBridge) RelayPoll(ctx context.Context, _ string) ([]workspacebridge.RelayEvent, error) {
	select {
	case events := <-b.events:
		return events, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *fakeBridge) TerminalOutput(_ context.Context, _, terminal string, data []byte, state string, exit *int, failure string) error {
	b.mu.Lock()
	b.outputs = append(b.outputs, output{terminal, state, failure, append([]byte(nil), data...), exit})
	b.mu.Unlock()
	b.changed <- struct{}{}
	return nil
}

// waitFor returns once check holds over everything sent so far.
func (b *fakeBridge) waitFor(t *testing.T, check func([]output) bool) []output {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		b.mu.Lock()
		got := append([]output(nil), b.outputs...)
		b.mu.Unlock()
		if check(got) {
			return got
		}
		select {
		case <-b.changed:
		case <-deadline:
			t.Fatalf("timed out; sent %d outputs", len(got))
		}
	}
}

func closedWith(id string) func([]output) bool {
	return func(outs []output) bool {
		for _, o := range outs {
			if o.terminal == id && o.state == "closed" {
				return true
			}
		}
		return false
	}
}

func TestHostRefusesWhenNotAllowedOrOutsideTheRoot(t *testing.T) {
	root := t.TempDir()
	allowed := false
	bridge := newFakeBridge()
	host := &Host{Bridge: bridge, WorkspaceID: "ws", Settings: func() (bool, string, error) { return allowed, root, nil },
		Start: func(string, int, int) (Process, error) { t.Fatal("started a shell"); return nil, nil }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go host.Run(ctx)
	bridge.events <- []workspacebridge.RelayEvent{{Type: "open", Terminal: "t1", Cols: 80, Rows: 24}}
	outs := bridge.waitFor(t, closedWith("t1"))
	if outs[0].failure != "terminal_not_allowed" {
		t.Fatalf("failure %q", outs[0].failure)
	}
	allowed = true
	bridge.events <- []workspacebridge.RelayEvent{{Type: "open", Terminal: "t2", Cols: 80, Rows: 24, Cwd: t.TempDir()}}
	outs = bridge.waitFor(t, closedWith("t2"))
	if outs[len(outs)-1].failure != "terminal_cwd_outside_root" {
		t.Fatalf("failure %q", outs[len(outs)-1].failure)
	}
}

func TestHostRunsARealShellInTheRequestedDirectory(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("pseudo-terminals are Linux and macOS")
	}
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", "/bin/sh")
	bridge := newFakeBridge()
	host := &Host{Bridge: bridge, WorkspaceID: "ws", Settings: func() (bool, string, error) { return true, root, nil }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go host.Run(ctx)
	bridge.events <- []workspacebridge.RelayEvent{{Type: "open", Terminal: "t1", Cols: 100, Rows: 30, Cwd: project}}
	bridge.waitFor(t, func(outs []output) bool { return len(outs) > 0 && outs[0].state == "open" })
	typed := "printf 'cols=%s\\n' \"$(stty size)\"; pwd; echo \"term=$TERM\"; exit 3\n"
	bridge.events <- []workspacebridge.RelayEvent{{Type: "input", Terminal: "t1", Data: base64.StdEncoding.EncodeToString([]byte(typed))}}
	outs := bridge.waitFor(t, closedWith("t1"))
	var text strings.Builder
	for _, o := range outs {
		text.Write(o.data)
	}
	realProject, _ := filepath.EvalSymlinks(project)
	for _, want := range []string{"cols=30 100", realProject, "term=xterm-256color"} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, text.String())
		}
	}
	last := outs[len(outs)-1]
	if last.exit == nil || *last.exit != 3 {
		t.Fatalf("exit %v", last.exit)
	}
}

func TestHostClosesTheShellWhenTheBrowserDoes(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("pseudo-terminals are Linux and macOS")
	}
	root := t.TempDir()
	t.Setenv("SHELL", "/bin/sh")
	bridge := newFakeBridge()
	host := &Host{Bridge: bridge, WorkspaceID: "ws", Settings: func() (bool, string, error) { return true, root, nil }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go host.Run(ctx)
	bridge.events <- []workspacebridge.RelayEvent{{Type: "open", Terminal: "t1", Cols: 80, Rows: 24}}
	bridge.waitFor(t, func(outs []output) bool { return len(outs) > 0 && outs[0].state == "open" })
	bridge.events <- []workspacebridge.RelayEvent{{Type: "close", Terminal: "t1"}}
	bridge.waitFor(t, closedWith("t1"))
	if host.get("t1") != nil {
		t.Fatal("shell still tracked")
	}
}
