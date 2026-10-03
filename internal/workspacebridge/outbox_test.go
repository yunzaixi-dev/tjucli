package workspacebridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/yunzaixi-dev/tjucli/internal/workspaceconfig"
)

func privateConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := workspaceconfig.EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDurableCompletionRecoveryNeverRepeatsExecution(t *testing.T) {
	var available atomic.Bool
	var polls, executions, completions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/poll"):
			polls.Add(1)
			_, _ = w.Write([]byte(`{"invocation":{"id":"job","target_workspace_id":"target","status":"running","capability":"pi.prompt","arguments":{}}}`))
		case strings.HasSuffix(r.URL.Path, "/complete"):
			completions.Add(1)
			if !available.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"invocation":{"id":"job","status":"succeeded"}}`))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	client, _ := New(server.URL, "secret")
	dir := privateConfigDir(t)
	outbox, err := client.CompletionOutbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := client.StepDurable(context.Background(), "target", func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		executions.Add(1)
		return json.RawMessage(`{"output":"result survives restart"}`), nil
	}, outbox)
	if !claimed || err == nil {
		t.Fatalf("wanted durable failed delivery: claimed=%v err=%v", claimed, err)
	}
	path := filepath.Join(outbox.dir, "job.json")
	if err := workspaceconfig.CheckPrivateFile(path); err != nil {
		t.Fatalf("private completion missing: %v", err)
	}
	// Construct a fresh outbox like a restarted connector; flush has no executor.
	available.Store(true)
	recovered, err := client.CompletionOutbox(dir)
	if err != nil || recovered.Flush(context.Background(), client, "target") != nil {
		t.Fatalf("recovery failed: %v", err)
	}
	if polls.Load() != 1 || executions.Load() != 1 || completions.Load() != 2 {
		t.Fatalf("replayed: polls=%d executions=%d completions=%d", polls.Load(), executions.Load(), completions.Load())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("acknowledged completion retained: %v", err)
	}
}

func TestLostCompletionResponseReconcilesExactResult(t *testing.T) {
	var matches atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/complete") {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"id":"workspace_invocation_conflict"}}`))
			return
		}
		value := "different"
		if matches.Load() {
			value = "done"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"invocation": map[string]any{
			"id": "job", "target_workspace_id": "target", "status": "succeeded",
			"result": map[string]string{"output": value},
		}})
	}))
	defer server.Close()
	client, _ := New(server.URL, "secret")
	outbox, _ := client.CompletionOutbox(privateConfigDir(t))
	if err := outbox.save(pendingCompletion{WorkspaceID: "target", InvocationID: "job", Result: json.RawMessage(`{ "output": "done" }`)}); err != nil {
		t.Fatal(err)
	}
	if err := outbox.Flush(context.Background(), client, "target"); err == nil {
		t.Fatal("conflicting result silently accepted")
	}
	matches.Store(true)
	if err := outbox.Flush(context.Background(), client, "target"); err != nil {
		t.Fatal(err)
	}
}

func TestOutboxRejectsChangedConnectionWithoutSendingResult(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, _ := New(server.URL, "original-test-token")
	outbox, err := client.CompletionOutbox(privateConfigDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := outbox.save(pendingCompletion{WorkspaceID: "target", InvocationID: "job", Result: json.RawMessage(`{"output":"fixture"}`)}); err != nil {
		t.Fatal(err)
	}
	changedToken, _ := New(server.URL, "changed-test-token")
	changedOrigin, _ := New(server.URL+"/other-api", "original-test-token")
	for _, changed := range []*Client{changedToken, changedOrigin, nil} {
		if outbox.Flush(context.Background(), changed, "target") == nil || requests.Load() != 0 {
			t.Fatal("outbox sent captured result through a changed connection")
		}
	}
	if err := workspaceconfig.CheckPrivateFile(filepath.Join(outbox.dir, "job.json")); err != nil {
		t.Fatal("connection mismatch erased undelivered result")
	}
}

func TestRevokedOutboxDeliveryRetainsResultWithoutReexecution(t *testing.T) {
	var requests, polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if strings.HasSuffix(r.URL.Path, "/poll") {
			polls.Add(1)
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	client, _ := New(server.URL, "revoked-test-token")
	outbox, err := client.CompletionOutbox(privateConfigDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := outbox.save(pendingCompletion{WorkspaceID: "target", InvocationID: "job", Result: json.RawMessage(`{"output":"fixture"}`)}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(outbox.dir, "job.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := outbox.Flush(context.Background(), client, "target"); err == nil || strings.Contains(err.Error(), "revoked-test-token") {
			t.Fatal("revoked delivery succeeded or disclosed credentials")
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) || requests.Load() != 4 || polls.Load() != 0 {
		t.Fatal("revoked recovery lost result or polled new work")
	}
}

func TestOutboxIsBoundToConnectionAndRejectsUnsafeStorage(t *testing.T) {
	dir := privateConfigDir(t)
	first, _ := New("https://example.com/api", "first")
	second, _ := New("https://example.com/api", "second")
	a, err := first.CompletionOutbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.CompletionOutbox(dir)
	if err != nil || a.dir == b.dir || strings.Contains(a.dir, "first") {
		t.Fatalf("unbound/secret-bearing outbox: %v", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if _, err := first.CompletionOutbox(dir); err == nil {
			t.Fatal("accepted public result directory")
		}
		if err := workspaceconfig.EnsurePrivateDir(dir); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err == nil {
		if _, err := first.CompletionOutbox(alias); err == nil {
			t.Fatal("accepted symlinked result directory")
		}
	}
	if err := os.WriteFile(filepath.Join(a.dir, "job.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := a.Flush(context.Background(), first, "target"); err == nil {
		t.Fatal("accepted public result file")
	}
}

func TestOutboxFlushAcceptsExactlyCapacityWithoutExecution(t *testing.T) {
	var requests, polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/poll") {
			polls.Add(1)
		}
		if !strings.HasSuffix(r.URL.Path, "/complete") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, _ := New(server.URL, "test-only-token")
	outbox, err := client.CompletionOutbox(privateConfigDir(t))
	if err != nil {
		t.Fatal(err)
	}
	for n := range maxOutboxEntries {
		item := pendingCompletion{WorkspaceID: "target", InvocationID: fmt.Sprintf("job%03d", n), Result: json.RawMessage(`{}`)}
		if err := outbox.save(item); err != nil {
			t.Fatalf("could not fill outbox entry %d: %v", n, err)
		}
		if err := workspaceconfig.CheckPrivateFile(filepath.Join(outbox.dir, item.InvocationID+".json")); err != nil {
			t.Fatal("outbox entry is not private")
		}
	}
	if err := outbox.save(pendingCompletion{WorkspaceID: "target", InvocationID: "overflow", Result: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("save exceeded capacity")
	}
	if err := outbox.Flush(context.Background(), client, "target"); err != nil {
		t.Fatalf("Flush rejected exactly capacity: %v", err)
	}
	if err := outbox.Flush(context.Background(), client, "target"); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != maxOutboxEntries || polls.Load() != 0 {
		t.Fatal("Flush did not deliver each result exactly once, or polled new work")
	}
	entries, err := os.ReadDir(outbox.dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("successful Flush left results or temporary files")
	}
}

func TestOutboxFlushRejectsOverCapacityBeforeNetwork(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, _ := New(server.URL, "test-only-token")
	outbox, err := client.CompletionOutbox(privateConfigDir(t))
	if err != nil {
		t.Fatal(err)
	}
	for n := range maxOutboxEntries + 1 {
		// Crash artifacts count toward the storage bound, but are never sent.
		path := filepath.Join(outbox.dir, fmt.Sprintf(".pending-%03d", n))
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if err := workspaceconfig.EnsurePrivateFile(path); err != nil {
			f.Close()
			t.Fatal(err)
		}
		f.Close()
	}
	if err := outbox.Flush(context.Background(), client, "target"); err == nil || requests.Load() != 0 {
		t.Fatal("oversized outbox was accepted or contacted server")
	}
	entries, _ := os.ReadDir(outbox.dir)
	if len(entries) != maxOutboxEntries+1 {
		t.Fatal("Flush erased crash artifacts")
	}
}

func TestOutboxFullCrashArtifactsRejectReadiness(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, _ := New(server.URL, "test-only-token")
	outbox, err := client.CompletionOutbox(privateConfigDir(t))
	if err != nil {
		t.Fatal(err)
	}
	for n := range maxOutboxEntries {
		path := filepath.Join(outbox.dir, fmt.Sprintf(".pending-%03d", n))
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if err := workspaceconfig.EnsurePrivateFile(path); err != nil {
			f.Close()
			t.Fatal(err)
		}
		f.Close()
	}
	if outbox.Flush(context.Background(), client, "target") == nil || requests.Load() != 0 {
		t.Fatal("full crash-artifact outbox reported readiness or contacted server")
	}
	var executions atomic.Int32
	claimed, err := client.StepDurable(context.Background(), "target",
		func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
			executions.Add(1)
			return json.RawMessage(`{}`), nil
		}, outbox)
	if err == nil || claimed || requests.Load() != 0 || executions.Load() != 0 {
		t.Fatal("full crash-artifact outbox claimed or executed new work")
	}
	entries, err := os.ReadDir(outbox.dir)
	if err != nil || len(entries) != maxOutboxEntries {
		t.Fatal("recovery erased crash artifacts")
	}
}

func TestOutboxUnixWeakenedDirectoriesRejectWithoutRepair(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native Windows tests use explicit DACL fixtures")
	}
	for _, level := range []string{"config", "parent", "connection"} {
		t.Run(level, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			client, _ := New(server.URL, "test-only-token")
			dir := privateConfigDir(t)
			outbox, err := client.CompletionOutbox(dir)
			if err != nil {
				t.Fatal(err)
			}
			item := pendingCompletion{WorkspaceID: "target", InvocationID: "job", Result: json.RawMessage(`{}`)}
			if err := outbox.save(item); err != nil {
				t.Fatal(err)
			}
			path := dir
			if level == "parent" {
				path = filepath.Dir(outbox.dir)
			} else if level == "connection" {
				path = outbox.dir
			}
			if err := os.Chmod(path, 0755); err != nil {
				t.Fatal(err)
			}
			if outbox.Flush(context.Background(), client, "target") == nil ||
				outbox.save(item) == nil || requests.Load() != 0 {
				t.Fatal("outbox accepted weakened storage or contacted server")
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0755 {
				t.Fatal("outbox silently repaired weakened storage")
			}
			if _, err := os.Stat(filepath.Join(outbox.dir, "job.json")); err != nil {
				t.Fatal("outbox erased undelivered result")
			}
		})
	}
}

func TestOutboxRejectsSymlinkDirectoriesFilesAndBoundedReads(t *testing.T) {
	for _, kind := range []string{"parent", "connection", "result", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			client, _ := New("https://example.invalid/api", "test-only-token")
			configDir := privateConfigDir(t)
			outbox, err := client.CompletionOutbox(configDir)
			if err != nil {
				t.Fatal(err)
			}
			outside := privateConfigDir(t)
			target := filepath.Join(outside, "untouched")
			if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "parent":
				parent := filepath.Dir(outbox.dir)
				if err := os.Remove(outbox.dir); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(parent); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, parent); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			case "connection":
				if err := os.Remove(outbox.dir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, outbox.dir); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			case "result":
				if err := os.Symlink(target, filepath.Join(outbox.dir, "job.json")); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			case "oversized":
				path := filepath.Join(outbox.dir, "job.json")
				if err := os.WriteFile(path, []byte(strings.Repeat("x", maxResultBytes+1025)), 0600); err != nil {
					t.Fatal(err)
				}
				if err := workspaceconfig.EnsurePrivateFile(path); err != nil {
					t.Fatal(err)
				}
			}
			if err := outbox.Flush(context.Background(), client, "target"); err == nil {
				t.Fatal("unsafe outbox was accepted")
			}
			if kind == "parent" || kind == "connection" {
				if _, err := client.CompletionOutbox(configDir); err == nil {
					t.Fatal("constructor accepted a symlinked outbox")
				}
				if err := outbox.save(pendingCompletion{WorkspaceID: "target", InvocationID: "job", Result: json.RawMessage(`{}`)}); err == nil {
					t.Fatal("save followed a symlinked directory")
				}
			}
			data, _ := os.ReadFile(target)
			if string(data) != "untouched" {
				t.Fatal("outbox modified external symlink target")
			}
		})
	}
}

func TestConcurrentOutboxInstancesCannotOverwriteCapturedResult(t *testing.T) {
	client, _ := New("https://example.invalid/api", "test-only-token")
	dir := privateConfigDir(t)
	first, err := client.CompletionOutbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.CompletionOutbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 2)
	for n, outbox := range []*CompletionOutbox{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- outbox.save(pendingCompletion{WorkspaceID: "target", InvocationID: "job", Result: json.RawMessage(fmt.Sprintf(`{"captured":%d}`, n))})
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	success := 0
	for err := range errs {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatal("concurrent outbox publication overwrote or lost captured result")
	}
	if err := workspaceconfig.CheckPrivateFile(filepath.Join(first.dir, "job.json")); err != nil {
		t.Fatal("concurrent publication lost privacy")
	}
	entries, _ := os.ReadDir(first.dir)
	if len(entries) != 1 {
		t.Fatal("concurrent publication left temporary files")
	}
}

func TestDurableFailureDoesNotPersistDiagnostics(t *testing.T) {
	client, _ := New("https://example.com", "secret")
	outbox, _ := client.CompletionOutbox(privateConfigDir(t))
	item := pendingCompletion{WorkspaceID: "target", InvocationID: "job", Failed: true, Result: json.RawMessage(`"provider-password"`)}
	if err := outbox.save(item); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(outbox.dir, "job.json"))
	if err != nil || strings.Contains(string(data), "password") {
		t.Fatalf("diagnostics persisted: %v", err)
	}
	if err := outbox.save(item); err == nil {
		t.Fatal("overwrote existing durable result")
	}
	if _, err := client.StepDurable(context.Background(), "target", nil, nil); err == nil {
		t.Fatal("accepted nil outbox")
	}
	if completionMatches(item, &Invocation{Status: "failed", Error: &RemoteError{ID: errors.New("other").Error()}}) {
		t.Fatal("accepted unrelated failure")
	}
}
