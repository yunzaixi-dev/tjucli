package workspaceruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeApprovalHooksRequireOneExplicitOwnerReply(t *testing.T) {
	for _, runtime := range []string{"claude", "codex"} {
		for _, decision := range []ApprovalDecision{ApprovalAllow, ApprovalDeny} {
			t.Run(runtime+"/"+string(decision), func(t *testing.T) {
				e := fakeExecutor(t, runtime+".prompt")
				setMode(t, e, "approval")
				count := 0
				e.Approvals = ApprovalFunc(func(ctx context.Context, request ApprovalRequest) (ApprovalDecision, error) {
					count++
					if request.Runtime != runtime || !sessionIDPattern.MatchString(request.ID) ||
						!sessionIDPattern.MatchString(request.SessionID) ||
						strings.Contains(string(request.Input), "fake-secret") || !strings.Contains(string(request.Input), "[redacted]") {
						t.Error("unsafe local approval display")
					}
					return decision, nil
				})
				_, err := e.Execute(context.Background(), runtime+".prompt", json.RawMessage(`{"prompt":"ask owner"}`))
				if decision == ApprovalAllow && err != nil {
					t.Fatal("explicit owner approval rejected", err)
				}
				if decision == ApprovalDeny && !errors.Is(err, ErrApprovalRequired) {
					t.Fatal("owner denial bypassed", err)
				}
				if count != 1 {
					t.Fatal("unexpected approval count", count)
				}
				record := readRecord(t, e)
				encoded, _ := json.Marshal(record.RPC)
				if strings.Contains(string(encoded), "acceptForSession") || strings.Contains(string(encoded), "updatedPermissions") {
					t.Fatal("approval persisted broader permission rules")
				}
				if decision == ApprovalAllow && runtime == "claude" &&
					!strings.Contains(string(encoded), `"command\":\"echo fake-secret`) &&
					!strings.Contains(string(encoded), `"command":"echo fake-secret"`) {
					t.Fatal("Claude one-shot approval changed original tool input")
				}
			})
		}
	}
}

func TestNativeApprovalCancellationAndUnknownRequestsFailClosed(t *testing.T) {
	for _, runtime := range []string{"claude", "codex"} {
		t.Run(runtime, func(t *testing.T) {
			e := fakeExecutor(t, runtime+".prompt")
			setMode(t, e, "approval")
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			e.Approvals = ApprovalFunc(func(ctx context.Context, request ApprovalRequest) (ApprovalDecision, error) {
				<-ctx.Done()
				return ApprovalAllow, nil // late approval must not be used
			})
			if _, err := e.Execute(ctx, runtime+".prompt", json.RawMessage(`{"prompt":"ask"}`)); !errors.Is(err, ErrCanceled) {
				t.Fatal("cancellation used late approval", err)
			}
			setMode(t, e, "unknown-request")
			called := false
			e.Approvals = ApprovalFunc(func(context.Context, ApprovalRequest) (ApprovalDecision, error) {
				called = true
				return ApprovalAllow, nil
			})
			if _, err := e.Execute(context.Background(), runtime+".prompt", json.RawMessage(`{"prompt":"ask"}`)); !errors.Is(err, ErrApprovalRequired) {
				t.Fatal("unknown native request accepted", err)
			}
			if called {
				t.Fatal("unknown native operation offered an approval bypass")
			}
		})
	}
}

func TestFileApprovalQueueNativeOwnerResponseAndCleanup(t *testing.T) {
	e := fakeExecutor(t, "codex.prompt")
	dir, err := e.ApprovalDirectory()
	if err != nil {
		t.Fatal(err)
	}
	e.Approvals = FileApprovals{Dir: dir, Timeout: 2 * time.Second}
	setMode(t, e, "approval")
	done := make(chan error, 1)
	go func() {
		_, err := e.Execute(context.Background(), "codex.prompt", json.RawMessage(`{"prompt":"ask"}`))
		done <- err
	}()
	var request ApprovalRequest
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		items, err := e.PendingApprovals(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(items) > 0 {
			request = items[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if request.ID == "" {
		t.Fatal("native approval not published")
	}
	info, err := os.Stat(filepath.Join(dir, request.ID, "request.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("approval request not private")
	}
	if err := e.RespondApproval(context.Background(), request.ID, ApprovalAllow); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal("owner file response not routed", err)
	}
	if _, err := os.Stat(filepath.Join(dir, request.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("consumed owner response retained")
	}
	if err := e.RespondApproval(context.Background(), request.ID, ApprovalAllow); !errors.Is(err, ErrApprovalRequired) {
		t.Fatal("stale reply accepted", err)
	}
}

func TestFileApprovalDenialTimeoutCancellationAndMismatchedID(t *testing.T) {
	for _, mode := range []string{"deny", "timeout", "cancel", "wrong-id"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			id, _ := randomID()
			request := ApprovalRequest{ID: id, Runtime: "claude", Input: json.RawMessage(`{}`)}
			transport := FileApprovals{Dir: dir, Timeout: 80 * time.Millisecond}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan ApprovalDecision, 1)
			go func() {
				decision, _ := transport.RequestApproval(ctx, request)
				done <- decision
			}()
			if mode == "deny" || mode == "wrong-id" {
				for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
					if _, err := os.Stat(filepath.Join(dir, id, "request.json")); err == nil {
						break
					}
					time.Sleep(time.Millisecond)
				}
				replyID := id
				if mode == "wrong-id" {
					replyID, _ = randomID()
				}
				if atomicPrivateJSON(filepath.Join(dir, id, "response.json"),
					ApprovalResponse{RequestID: replyID, Decision: ApprovalDeny}) != nil {
					t.Fatal("response fixture")
				}
			} else if mode == "cancel" {
				cancel()
			}
			if decision := <-done; decision != ApprovalDeny {
				t.Fatal("missing/denied owner reply approved")
			}
			if _, err := os.Stat(filepath.Join(dir, id)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("expired approval retained")
			}
		})
	}
}

func TestStdioApprovalsBoundedLocalProtocolAndCancellation(t *testing.T) {
	for _, mode := range []string{"allow", "deny", "wrong-id", "timeout", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			bridge, owner := net.Pipe()
			defer bridge.Close()
			defer owner.Close()
			transport := &StdioApprovals{Stream: bridge, Timeout: 80 * time.Millisecond}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			id, _ := randomID()
			go func() {
				scanner := bufio.NewScanner(owner)
				if !scanner.Scan() {
					return
				}
				var request ApprovalRequest
				if json.Unmarshal(scanner.Bytes(), &request) != nil || request.ID != id {
					return
				}
				if mode == "timeout" {
					return
				}
				if mode == "cancel" {
					cancel()
					return
				}
				decision := ApprovalDeny
				if mode == "allow" {
					decision = ApprovalAllow
				}
				if mode == "wrong-id" {
					request.ID, _ = randomID()
				}
				_ = json.NewEncoder(owner).Encode(ApprovalResponse{RequestID: request.ID, Decision: decision})
			}()
			decision, err := transport.RequestApproval(ctx, ApprovalRequest{ID: id, Input: json.RawMessage(`{}`)})
			if mode == "allow" {
				if err != nil || decision != ApprovalAllow {
					t.Fatal("local stdio approval rejected", err)
				}
			} else if decision != ApprovalDeny {
				t.Fatal("bad/timed out/canceled local reply approved")
			}
		})
	}
}
