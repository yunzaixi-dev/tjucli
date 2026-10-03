package workspaceruntime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/yunzaixi-dev/tjucli/internal/workspaceconfig"
)

var (
	ErrSessionUnavailable = errors.New("workspace_session_unavailable")
	ErrSessionBusy        = errors.New("workspace_session_busy")
	ErrSessionLimit       = errors.New("workspace_session_limit")
	sessionIDPattern      = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$`)
)

const (
	maxSessions = 1024
	maxHistory  = 32 << 20
)

// SessionInfo deliberately excludes native IDs, paths, prompts and auth data.
type SessionInfo struct {
	ID        string    `json:"id"`
	Runtime   string    `json:"runtime"`
	Endpoint  string    `json:"endpoint"`
	State     string    `json:"state"` // ready, running, interrupted
	Turns     int       `json:"turns"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type conversation struct {
	SessionInfo
	WorkspaceID string `json:"workspace_id"`
	Root        string `json:"root"`
	Binding     string `json:"binding"`
	NativeID    string `json:"native_id,omitempty"`
	dir         string
}

func randomID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", ErrRuntimeFailed
	}
	data[6], data[8] = (data[6]&15)|64, (data[8]&63)|128
	return fmt.Sprintf("%x-%x-%x-%x-%x", data[0:4], data[4:6], data[6:8], data[8:10], data[10:]), nil
}

// atomicPrivateJSON is only used beneath verified private state directories.
func atomicPrivateJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil || len(data) > maxArguments {
		return ErrInvalidConfig
	}
	return privateBytes(path, data)
}

func readPrivateFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit ||
		workspaceconfig.CheckPrivateFile(path) != nil {
		return nil, ErrInvalidConfig
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrInvalidConfig
	}
	defer f.Close()
	current, err := f.Stat()
	if err != nil || !os.SameFile(info, current) {
		return nil, ErrInvalidConfig
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, ErrInvalidConfig
	}
	return data, nil
}

func (e Executor) conversationBase(runtime string) (string, error) {
	if !slices.Contains([]string{"pi", "claude", "codex"}, runtime) ||
		!slices.Contains(e.Config.AllowedCapabilities, runtime+".prompt") {
		return "", ErrCapabilityDenied
	}
	canonical, err := filepath.EvalSymlinks(e.StateDir)
	if !filepath.IsAbs(e.StateDir) || err != nil || canonical != filepath.Clean(e.StateDir) {
		return "", ErrInvalidConfig
	}
	dir := filepath.Join(e.StateDir, "runtime")
	for _, part := range []string{"", "conversations", runtime} {
		dir = filepath.Join(dir, part)
		if privateDir(dir) != nil {
			return "", ErrInvalidConfig
		}
	}
	return dir, nil
}

func (e Executor) openConversation(ctx context.Context, runtime, id, endpoint, binding string) (*conversation, func(), error) {
	base, err := e.conversationBase(runtime)
	if err != nil {
		return nil, nil, err
	}
	// Serialize creation/capacity checks across CLI processes, but not turns
	// belonging to distinct conversations.
	unlockBase, err := lockSession(ctx, filepath.Join(base, ".lock"))
	if err != nil {
		return nil, nil, err
	}
	defer unlockBase()
	fresh := id == ""
	if fresh {
		entries, err := os.ReadDir(base)
		if err != nil || len(entries) > maxSessions {
			return nil, nil, ErrSessionLimit
		}
		id, err = randomID()
		if err != nil {
			return nil, nil, err
		}
	} else if !sessionIDPattern.MatchString(id) {
		return nil, nil, ErrInvalidArguments
	}
	dir := filepath.Join(base, id)
	if fresh {
		if os.Mkdir(dir, 0700) != nil || privateDir(dir) != nil {
			return nil, nil, ErrInvalidConfig
		}
		if syncPrivateParent(dir) != nil {
			return nil, nil, ErrInvalidConfig
		}
	} else {
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
			workspaceconfig.CheckPrivateDir(dir) != nil {
			return nil, nil, ErrSessionUnavailable
		}
	}
	unlock, err := lockSession(ctx, filepath.Join(dir, ".lock"))
	if err != nil {
		return nil, nil, err
	}
	c := &conversation{dir: dir}
	if fresh {
		now := time.Now().UTC()
		c.SessionInfo = SessionInfo{ID: id, Runtime: runtime, Endpoint: endpoint, State: "ready", CreatedAt: now, UpdatedAt: now}
		c.WorkspaceID, c.Root, c.Binding = e.Config.ID, e.Config.Root, binding
	} else {
		raw, err := readPrivateFile(filepath.Join(dir, "metadata.json"), maxArguments)
		if err != nil || json.Unmarshal(raw, c) != nil || c.ID != id ||
			c.Runtime != runtime || c.WorkspaceID != e.Config.ID || c.Root != e.Config.Root ||
			c.Binding != binding || c.Endpoint != endpoint || c.Turns < 0 {
			unlock()
			return nil, nil, ErrSessionUnavailable
		}
	}
	for _, name := range []string{"pi", "claude", "codex"} {
		if privateDir(filepath.Join(dir, name)) != nil {
			unlock()
			return nil, nil, ErrInvalidConfig
		}
	}
	if err := c.checkFiles(); err != nil {
		unlock()
		return nil, nil, err
	}
	// A prior crash may have left running metadata. Acquiring the OS lock
	// proves no live turn owns it. Resuming submits ONLY the new caller prompt,
	// never automatically replays a delivered/partially executed turn.
	c.State = "running"
	if c.save() != nil {
		unlock()
		return nil, nil, ErrInvalidConfig
	}
	return c, unlock, nil
}

func bindingHash(values ...any) string {
	data, _ := json.Marshal(values)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (c *conversation) save() error {
	c.UpdatedAt = time.Now().UTC()
	return atomicPrivateJSON(filepath.Join(c.dir, "metadata.json"), c)
}

func (c *conversation) finish(success bool) error {
	c.State = "interrupted"
	if success {
		c.State = "ready"
		c.Turns++
	}
	return c.save()
}

func (c *conversation) checkFiles() error {
	var size int64
	count := 0
	return filepath.WalkDir(c.dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.Type()&os.ModeSymlink != 0 {
			return ErrSessionUnavailable
		}
		info, err := entry.Info()
		if err != nil {
			return ErrSessionUnavailable
		}
		count++
		if count > 4096 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return ErrSessionLimit
		}
		if info.IsDir() {
			if workspaceconfig.EnsurePrivateDir(path) != nil {
				return ErrInvalidConfig
			}
		} else {
			size += info.Size()
			if size > maxHistory {
				return ErrSessionLimit
			}
			if workspaceconfig.EnsurePrivateFile(path) != nil {
				return ErrInvalidConfig
			}
		}
		file, err := os.Open(path)
		if err != nil {
			return ErrInvalidConfig
		}
		if info.IsDir() {
			err = syncPrivateDirectory(file)
		} else {
			err = file.Sync()
		}
		file.Close()
		if err != nil {
			return ErrInvalidConfig
		}
		return nil
	})
}

// ListSessions is a local metadata API, not a new remotely published capability.
// Parent CLI/native wiring must not expose arbitrary StateDir or runtime paths.
func (e Executor) ListSessions(ctx context.Context, runtime string) ([]SessionInfo, error) {
	if ctx == nil {
		return nil, ErrInvalidArguments
	}
	if runtime == "" {
		all := []SessionInfo{}
		for _, name := range []string{"pi", "claude", "codex"} {
			if !slices.Contains(e.Config.AllowedCapabilities, name+".prompt") {
				continue
			}
			items, err := e.ListSessions(ctx, name)
			if err != nil {
				return nil, err
			}
			all = append(all, items...)
		}
		slices.SortFunc(all, func(a, b SessionInfo) int { return b.UpdatedAt.Compare(a.UpdatedAt) })
		return all, nil
	}
	base, err := e.conversationBase(runtime)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) > maxSessions+1 {
		return nil, ErrSessionLimit
	}
	result := []SessionInfo{}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return nil, ErrCanceled
		}
		if !sessionIDPattern.MatchString(entry.Name()) || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		raw, err := readPrivateFile(filepath.Join(base, entry.Name(), "metadata.json"), maxArguments)
		var c conversation
		if err != nil || json.Unmarshal(raw, &c) != nil || c.ID != entry.Name() ||
			c.Runtime != runtime || c.WorkspaceID != e.Config.ID {
			continue
		}
		if c.State == "running" {
			unlock, err := lockSession(ctx, filepath.Join(base, entry.Name(), ".lock"))
			if err == nil {
				c.State = "interrupted" // stale metadata, no live process lock
				unlock()
			}
		}
		result = append(result, c.SessionInfo)
	}
	slices.SortFunc(result, func(a, b SessionInfo) int { return b.UpdatedAt.Compare(a.UpdatedAt) })
	return result, nil
}

// Session returns safe local metadata only. Native transcripts/paths and auth
// files are never exposed by the CLI/native status contract.
func (e Executor) Session(ctx context.Context, id string) (SessionInfo, error) {
	if !sessionIDPattern.MatchString(id) {
		return SessionInfo{}, ErrInvalidArguments
	}
	items, err := e.ListSessions(ctx, "")
	if err != nil {
		return SessionInfo{}, err
	}
	for _, item := range items {
		if item.ID == id {
			return item, nil
		}
	}
	return SessionInfo{}, ErrSessionUnavailable
}
