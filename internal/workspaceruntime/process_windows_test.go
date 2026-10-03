//go:build windows

package workspaceruntime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/yunzaixi-dev/tjucli/internal/workspaceconfig"
)

func TestWindowsPromptGatePreservesLocalSessionAndApprovalMetadata(t *testing.T) {
	e := Executor{Config: workspaceconfig.Config{
		Version: 1, ID: "local-metadata", Name: "test", Root: t.TempDir(),
		AllowedCapabilities: []string{"codex.prompt"},
	}, StateDir: t.TempDir()}
	ctx := context.Background()
	base, err := e.conversationBase("codex")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := randomID()
	c := conversation{SessionInfo: SessionInfo{
		ID: id, Runtime: "codex", State: "ready",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}, WorkspaceID: e.Config.ID, dir: filepath.Join(base, id)}
	if privateDir(c.dir) != nil || c.save() != nil {
		t.Fatal("private persisted metadata fixture")
	}
	items, err := e.ListSessions(ctx, "codex")
	if err != nil || len(items) != 1 || items[0].ID != id {
		t.Fatal("prompt gate disabled session listing", err)
	}
	if item, err := e.Session(ctx, id); err != nil || item.ID != id {
		t.Fatal("prompt gate disabled session status", err)
	}
	dir, err := e.ApprovalDirectory()
	if err != nil {
		t.Fatal(err)
	}
	approvalID, _ := randomID()
	requestDir := filepath.Join(dir, approvalID)
	request := ApprovalRequest{ID: approvalID, Runtime: "codex", SessionID: id,
		Kind: "command", Input: json.RawMessage(`{"command":"synthetic command"}`),
		ExpiresAt: time.Now().Add(approvalWait)}
	if privateDir(requestDir) != nil ||
		atomicPrivateJSON(filepath.Join(requestDir, "request.json"), request) != nil {
		t.Fatal("private approval metadata fixture")
	}
	pending, err := e.PendingApprovals(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != approvalID {
		t.Fatal("prompt gate disabled pending approvals", err)
	}
	if err := e.RespondApproval(ctx, approvalID, ApprovalDeny); err != nil {
		t.Fatal("prompt gate disabled local owner response", err)
	}
	pending, err = e.PendingApprovals(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatal("consumed response still pending", err)
	}
}
