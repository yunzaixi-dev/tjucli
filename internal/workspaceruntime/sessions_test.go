package workspaceruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sessionPrompt(t *testing.T, e Executor, runtime, id, prompt string) (string, string) {
	t.Helper()
	raw, _ := json.Marshal(promptArgs{Prompt: prompt, SessionID: id})
	result, err := e.Execute(context.Background(), runtime+".prompt", raw)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		SessionID string `json:"session_id"`
		Output    string `json:"output"`
	}
	if json.Unmarshal(result, &response) != nil || !sessionIDPattern.MatchString(response.SessionID) {
		t.Fatal("missing opaque session ID")
	}
	return response.SessionID, response.Output
}

func TestConversationsPersistAcrossExecutorsAndResumeNativeHistory(t *testing.T) {
	for _, runtime := range []string{"pi", "claude", "codex"} {
		t.Run(runtime, func(t *testing.T) {
			e := fakeExecutor(t, runtime+".prompt")
			setMode(t, e, "session-history")
			id, output := sessionPrompt(t, e, runtime, "", "first prompt")
			if output != "turn 1" {
				t.Fatal("new native conversation did not start independently")
			}
			restarted := Executor{Config: e.Config, StateDir: e.StateDir}
			resumedID, output := sessionPrompt(t, restarted, runtime, id, "second prompt")
			if resumedID != id || output != "turn 2" {
				t.Fatal("conversation did not resume native persisted history")
			}
			info, err := restarted.Session(context.Background(), id)
			if err != nil || info.Turns != 2 || info.State != "ready" {
				t.Fatal("durable session metadata", err)
			}
			record := readRecord(t, e)
			if runtime == "codex" && !strings.Contains(string(record.RPC[2]), `"method":"thread/resume"`) {
				t.Fatal("Codex did not resume durable native thread")
			}
			if runtime == "claude" && !slicesContain(record.Args, "--resume="+id) {
				t.Fatal("Claude did not resume exact session ID")
			}
			newID, output := sessionPrompt(t, e, runtime, "", "independent prompt")
			if newID == id || output != "turn 1" {
				t.Fatal("new request silently continued a previous session")
			}
			other := restarted
			other.StateDir = t.TempDir() // same cwd and config ID, different workspace state
			raw, _ := json.Marshal(promptArgs{Prompt: "do not run", SessionID: id})
			if _, err := other.Execute(context.Background(), runtime+".prompt", raw); !errors.Is(err, ErrSessionUnavailable) {
				t.Fatal("session crossed state-directory boundary", err)
			}
			restarted.Config.ID = "different-workspace"
			if _, err := restarted.Execute(context.Background(), runtime+".prompt", raw); !errors.Is(err, ErrSessionUnavailable) {
				t.Fatal("session crossed identity boundary", err)
			}
			items, err := e.ListSessions(context.Background(), "")
			if err != nil || len(items) != 2 {
				t.Fatal("session catalog", err)
			}
			public, _ := json.Marshal(items)
			if strings.Contains(string(public), e.Config.Root) || strings.Contains(string(public), "first prompt") ||
				strings.Contains(string(public), "native_id") {
				t.Fatal("session catalog leaks private execution context")
			}
		})
	}
}

func TestSessionBindingAndStrictSelection(t *testing.T) {
	e := fakeExecutor(t, "pi.prompt", "claude.prompt")
	id, _ := sessionPrompt(t, e, "pi", "", "first")
	for _, invalid := range []string{"", "../secret", e.Config.Root, "thread", "00000000-0000-0000-0000-000000000000"} {
		raw, _ := json.Marshal(map[string]string{"prompt": "not run", "session_id": invalid})
		if _, err := e.Execute(context.Background(), "pi.prompt", raw); !errors.Is(err, ErrInvalidArguments) {
			t.Fatal("invalid session selection accepted", err)
		}
	}
	raw, _ := json.Marshal(promptArgs{Prompt: "next", SessionID: id})
	if _, err := e.Execute(context.Background(), "claude.prompt", raw); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatal("Pi session crossed runtime boundary", err)
	}
	e.Config.Endpoints[0].BaseURL = "https://different.example.test/v1"
	if _, err := e.Execute(context.Background(), "pi.prompt", raw); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatal("session crossed endpoint binding", err)
	}
}

func TestSessionBusyCancellationAndExplicitResumeWithoutReplay(t *testing.T) {
	e := fakeExecutor(t, "pi.prompt")
	setMode(t, e, "hang")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := e.Execute(ctx, "pi.prompt", json.RawMessage(`{"prompt":"delivered once"}`))
		done <- err
	}()
	var id string
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		items, _ := e.ListSessions(context.Background(), "pi")
		if len(items) != 0 && items[0].State == "running" {
			id = items[0].ID
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if id == "" {
		cancel()
		t.Fatal("running session was not visible")
	}
	raw, _ := json.Marshal(promptArgs{Prompt: "not concurrent", SessionID: id})
	if _, err := e.Execute(context.Background(), "pi.prompt", raw); !errors.Is(err, ErrSessionBusy) {
		cancel()
		t.Fatal("concurrent native turn accepted", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, ErrCanceled) {
		t.Fatal("cancel did not propagate", err)
	}
	info, err := e.Session(context.Background(), id)
	if err != nil || info.State != "interrupted" || info.Turns != 0 {
		t.Fatal("canceled session status", err)
	}
	setMode(t, e, "")
	sessionPrompt(t, e, "pi", id, "explicit new prompt")
	if readRecord(t, e).Input != "explicit new prompt" {
		t.Fatal("resume replayed delivered prompt")
	}
}

func TestSessionRejectsSymlinksAndOversizeNativeHistory(t *testing.T) {
	e := fakeExecutor(t, "pi.prompt")
	id, _ := sessionPrompt(t, e, "pi", "", "first")
	file := filepath.Join(e.StateDir, "runtime", "conversations", "pi", id, "pi", "session.jsonl")
	original, _ := os.ReadFile(file)
	_ = os.Remove(file)
	target := filepath.Join(t.TempDir(), "foreign.jsonl")
	_ = os.WriteFile(target, original, 0600)
	_ = os.Symlink(target, file)
	raw, _ := json.Marshal(promptArgs{Prompt: "next", SessionID: id})
	if _, err := e.Execute(context.Background(), "pi.prompt", raw); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatal("native history symlink accepted", err)
	}
	_ = os.Remove(file)
	f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Truncate(maxHistory + 1)
	f.Close()
	if _, err := e.Execute(context.Background(), "pi.prompt", raw); !errors.Is(err, ErrSessionLimit) {
		t.Fatal("unbounded native history accepted", err)
	}
}

func TestOwnerTimeoutIsBoundedAndRespectsUpstream(t *testing.T) {
	e := fakeExecutor(t, "pi.prompt")
	e.RunTimeout = 25 * time.Hour
	if _, err := e.Execute(context.Background(), "pi.prompt", json.RawMessage(`{"prompt":"no"}`)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal("unbounded owner timeout accepted", err)
	}
	e.RunTimeout = time.Hour
	setMode(t, e, "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if _, err := e.Execute(ctx, "pi.prompt", json.RawMessage(`{"prompt":"no"}`)); !errors.Is(err, ErrCanceled) {
		t.Fatal("owner timeout overrode upstream deadline", err)
	}
	e.RunTimeout = 80 * time.Millisecond
	if _, err := e.Execute(context.Background(), "pi.prompt", json.RawMessage(`{"prompt":"no"}`)); !errors.Is(err, ErrCanceled) {
		t.Fatal("local ceiling ignored", err)
	}
}
