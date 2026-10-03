package workspacebridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/yunzaixi-dev/tjucli/internal/workspaceconfig"
)

const maxOutboxEntries = 128

// CompletionOutbox persists results, not commands. Recovery never executes an
// invocation again. A connection-specific directory prevents sending a result
// to a newly linked account or origin after configuration changes.
type CompletionOutbox struct {
	dir string
	mu  sync.Mutex
}

type pendingCompletion struct {
	WorkspaceID  string          `json:"workspace_id"`
	InvocationID string          `json:"invocation_id"`
	Result       json.RawMessage `json:"result,omitempty"`
	Failed       bool            `json:"failed,omitempty"`
}

func (c *Client) CompletionOutbox(configDir string) (*CompletionOutbox, error) {
	// The config directory is already private; refuse symlink aliases and a
	// weakened mode/ACL rather than publishing results into shared storage.
	if workspaceconfig.CheckPrivateDir(configDir) != nil {
		return nil, errors.New("workspace_outbox_unavailable")
	}
	hash := sha256.Sum256([]byte(c.baseURL + "\x00" + c.token))
	parent := filepath.Join(configDir, "completion-outbox")
	dir := filepath.Join(parent, hex.EncodeToString(hash[:]))
	for _, path := range []string{parent, dir} {
		_, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			err = workspaceconfig.EnsurePrivateDir(path)
		} else if err == nil {
			err = workspaceconfig.CheckPrivateDir(path)
		}
		if err != nil {
			return nil, errors.New("workspace_outbox_unavailable")
		}
	}
	return &CompletionOutbox{dir: dir}, nil
}

func (b *CompletionOutbox) save(item pendingCompletion) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !validID(item.WorkspaceID) || !validID(item.InvocationID) ||
		(!item.Failed && (!json.Valid(item.Result) || len(item.Result) > maxResultBytes)) {
		return errors.New("workspace_result_invalid")
	}
	if item.Failed {
		item.Result = nil // Never persist executor diagnostics.
	}
	root, err := b.open()
	if err != nil {
		return err
	}
	defer root.Close()
	entries, err := outboxEntries(root)
	if err != nil || len(entries) >= maxOutboxEntries {
		return errors.New("workspace_outbox_full")
	}
	name := item.InvocationID + ".json"
	if _, err := root.Lstat(name); !os.IsNotExist(err) {
		return errors.New("workspace_outbox_conflict")
	}
	data, err := json.Marshal(item)
	if err != nil {
		return errors.New("workspace_result_invalid")
	}
	suffix := make([]byte, 16)
	if _, err := rand.Read(suffix); err != nil {
		return errors.New("workspace_outbox_unavailable")
	}
	temp := ".pending-" + hex.EncodeToString(suffix)
	// Anchored, empty, exclusive creation. Apply actual privacy controls
	// BEFORE any result bytes are written (chmod alone is not a Windows ACL).
	file, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("workspace_outbox_unavailable")
	}
	defer root.Remove(temp)
	if err = workspaceconfig.EnsurePrivateFile(filepath.Join(b.dir, temp)); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return errors.New("workspace_outbox_unavailable")
	}
	// No-replace publication also protects results from another outbox
	// instance/process racing the earlier existence check.
	if err := root.Link(temp, name); err != nil {
		if errors.Is(err, os.ErrExist) {
			return errors.New("workspace_outbox_conflict")
		}
		return errors.New("workspace_outbox_unavailable")
	}
	if root.Remove(temp) != nil {
		return errors.New("workspace_outbox_unavailable")
	}
	// Sync publication metadata before acknowledging durable capture on Unix.
	// Windows uses real ACLs and file Sync, but this is not a claim of
	// power-loss-safe directory metadata publication on Windows.
	if runtime.GOOS != "windows" {
		directory, err := root.Open(".")
		if err != nil {
			return errors.New("workspace_outbox_unavailable")
		}
		syncErr := directory.Sync()
		closeErr := directory.Close()
		if syncErr != nil || closeErr != nil {
			return errors.New("workspace_outbox_unavailable")
		}
	}
	return nil
}

func (b *CompletionOutbox) open() (*os.Root, error) {
	// Do not repair a weakened directory while recovering secret results.
	for _, dir := range []string{filepath.Dir(filepath.Dir(b.dir)), filepath.Dir(b.dir)} {
		if workspaceconfig.CheckPrivateDir(dir) != nil {
			return nil, errors.New("workspace_outbox_unavailable")
		}
	}
	root, err := workspaceconfig.OpenExistingPrivateDir(b.dir)
	if err != nil {
		return nil, errors.New("workspace_outbox_unavailable")
	}
	return root, nil
}

// Read at most capacity+1 entries; oversized/corrupted directories do not
// trigger an unbounded allocation. Exactly capacity is valid for Flush.
func outboxEntries(root *os.Root) ([]os.DirEntry, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(maxOutboxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > maxOutboxEntries {
		return nil, errors.New("workspace_outbox_full")
	}
	return entries, nil
}

// Flush retries delivery only. A lost HTTP response can be reconciled by an
// authenticated GET of an exactly matching terminal result. Conflicting,
// missing or revoked records remain on disk for explicit recovery; we never
// erase the sole remaining result merely because the server rejected it.
func (b *CompletionOutbox) Flush(ctx context.Context, c *Client, workspaceID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c == nil {
		return errors.New("workspace_outbox_unavailable")
	}
	connection := sha256.Sum256([]byte(c.baseURL + "\x00" + c.token))
	if filepath.Base(b.dir) != hex.EncodeToString(connection[:]) {
		return errors.New("workspace_outbox_unavailable")
	}
	root, err := b.open()
	if err != nil {
		return err
	}
	defer root.Close()
	entries, err := outboxEntries(root)
	if err != nil {
		return errors.New("workspace_outbox_unavailable")
	}
	pending := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".pending-") {
			pending++
			continue // A crashed atomic write cannot be considered delivered.
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if entry.Name() != id+".json" || !validID(id) || entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return errors.New("workspace_outbox_invalid")
		}
		path := filepath.Join(b.dir, entry.Name())
		if workspaceconfig.CheckPrivateFile(path) != nil {
			return errors.New("workspace_outbox_invalid")
		}
		file, err := workspaceconfig.OpenPrivateFile(path)
		if err != nil {
			return errors.New("workspace_outbox_invalid")
		}
		info, err := file.Stat()
		if err != nil || info.Size() > maxResultBytes+1024 {
			file.Close()
			return errors.New("workspace_outbox_invalid")
		}
		data, err := io.ReadAll(io.LimitReader(file, maxResultBytes+1025))
		closeErr := file.Close()
		if err != nil || closeErr != nil || len(data) > maxResultBytes+1024 {
			return errors.New("workspace_outbox_invalid")
		}
		var item pendingCompletion
		if json.Unmarshal(data, &item) != nil || item.InvocationID != id ||
			item.WorkspaceID != workspaceID || (!item.Failed && (!json.Valid(item.Result) || len(item.Result) > maxResultBytes)) {
			return errors.New("workspace_outbox_invalid")
		}
		var executionErr error
		if item.Failed {
			executionErr = errors.New("workspace_execution_failed")
		}
		err = c.Complete(ctx, workspaceID, id, item.Result, executionErr)
		if err != nil {
			invocation, lookupErr := c.Get(ctx, id)
			if lookupErr != nil || invocation.TargetWorkspaceID != workspaceID ||
				!completionMatches(item, invocation) {
				return err
			}
		}
		current, statErr := root.Lstat(entry.Name())
		if statErr != nil || !os.SameFile(info, current) || root.Remove(entry.Name()) != nil {
			return errors.New("workspace_outbox_unavailable")
		}
	}
	if pending >= maxOutboxEntries {
		// Do not report readiness to claim new work if crash artifacts leave
		// no room to capture its result. Explicit local recovery is required.
		return errors.New("workspace_outbox_full")
	}
	return nil
}

func completionMatches(item pendingCompletion, invocation *Invocation) bool {
	if item.Failed {
		return invocation.Status == "failed" && invocation.Error != nil &&
			invocation.Error.ID == "workspace_execution_failed"
	}
	if invocation.Status != "succeeded" {
		return false
	}
	// Compact insignificant whitespace without altering number precision.
	var result, expected bytes.Buffer
	if json.Compact(&result, invocation.Result) != nil || json.Compact(&expected, item.Result) != nil {
		return false
	}
	return bytes.Equal(result.Bytes(), expected.Bytes())
}
