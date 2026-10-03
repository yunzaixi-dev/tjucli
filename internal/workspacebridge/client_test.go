package workspacebridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRejectUnsafeConnections(t *testing.T) {
	for _, base := range []string{"http://example.com/api", "https://user:password@example.com/api", "https://example.com/api?token=secret", "https://example.com/arbitrary", "file:///tmp/config"} {
		if _, err := New(base, "secret"); err == nil {
			t.Fatalf("accepted %q", base)
		}
	}
	for _, id := range []string{"../other", "x/complete", "x?token", ""} {
		if validID(id) {
			t.Fatalf("accepted id %q", id)
		}
	}
}

func TestStepClaimsAndSanitizesError(t *testing.T) {
	completed := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer connector-secret" {
			t.Error("missing scoped token")
		}
		switch r.URL.Path {
		case "/api/workspaces/target/poll":
			_, _ = w.Write([]byte(`{"invocation":{"id":"job","target_workspace_id":"target","capability":"pi.prompt","arguments":{"prompt":"hi"},"status":"running"}}`))
		case "/api/workspaces/target/invocations/job/complete":
			completed++
			var body map[string]json.RawMessage
			if json.NewDecoder(r.Body).Decode(&body) != nil || strings.Contains(string(body["error"]), "secret") {
				t.Error("unsafe completion")
			}
			_, _ = w.Write([]byte(`{"invocation":{"id":"job","status":"failed"}}`))
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := New(server.URL+"/api", "connector-secret")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	claimed, err := client.Step(context.Background(), "target", func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		calls++
		return nil, errors.New("upstream URL and credential secret")
	})
	if err != nil || !claimed || completed != 1 || calls != 1 {
		t.Fatalf("step: claimed=%v err=%v completions=%d calls=%d", claimed, err, completed, calls)
	}
}

func TestDoesNotRedirectCredentials(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected = true }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client, _ := New(source.URL, "secret")
	err := client.Heartbeat(context.Background(), "target", nil)
	if err == nil || redirected || strings.Contains(err.Error(), "secret") {
		t.Fatalf("redirect unsafe: %v %v", err, redirected)
	}
}

func TestNoReplayOnCompletionFailure(t *testing.T) {
	polls, executions := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/poll") {
			polls++
			_, _ = w.Write([]byte(`{"invocation":{"id":"job","target_workspace_id":"target","status":"running","arguments":{},"capability":"pi.prompt"}}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client, _ := New(server.URL, "secret")
	_, err := client.Step(context.Background(), "target", func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		executions++
		return json.RawMessage(`{"text":"done"}`), nil
	})
	if err == nil || polls != 1 || executions != 1 {
		t.Fatalf("unexpected retry polls=%d executions=%d err=%v", polls, executions, err)
	}
}

func TestUntrustedErrorCodeCannotEchoCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"id": "connector-secret"}})
	}))
	defer server.Close()
	client, _ := New(server.URL, "connector-secret")
	err := client.Heartbeat(context.Background(), "target", nil)
	if err == nil || strings.Contains(err.Error(), "connector-secret") {
		t.Fatalf("unsafe untrusted error: %v", err)
	}
}

func TestPublicTimestampsArePreserved(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/workspaces" {
			_, _ = w.Write([]byte(`{"workspaces":[{"id":"target","name":"System","kind":"local","capabilities":[],"online":true,"last_seen_at":"2026-10-02T12:00:00Z","created_at":"2026-10-02T11:00:00Z"}]}`))
		} else {
			_, _ = w.Write([]byte(`{"invocation":{"id":"job","target_workspace_id":"target","status":"succeeded","created_at":"2026-10-02T12:00:00Z","updated_at":"2026-10-02T12:00:01Z"}}`))
		}
	}))
	defer server.Close()
	client, _ := New(server.URL, "secret")
	workspaces, err := client.List(context.Background())
	if err != nil || len(workspaces) != 1 || workspaces[0].LastSeenAt == nil || workspaces[0].CreatedAt.IsZero() {
		t.Fatalf("lost workspace timestamps: %v", err)
	}
	invocation, err := client.Get(context.Background(), "job")
	if err != nil || invocation.CreatedAt.IsZero() || invocation.UpdatedAt.IsZero() {
		t.Fatalf("lost invocation timestamps: %v", err)
	}
}

func TestInvocationTimeoutIsBoundedAndOptional(t *testing.T) {
	var timeouts []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Timeout int `json:"timeout_seconds"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid request")
		}
		timeouts = append(timeouts, body.Timeout)
		_, _ = w.Write([]byte(`{"invocation":{"id":"job","status":"queued"}}`))
	}))
	defer server.Close()
	client, _ := New(server.URL, "secret")
	if _, err := client.Invoke(context.Background(), "target", "pi.prompt", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.InvokeWithTimeout(context.Background(), "target", "pi.prompt", json.RawMessage(`{}`), 3600); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []int{-1, 86401} {
		if _, err := client.InvokeWithTimeout(context.Background(), "target", "pi.prompt", json.RawMessage(`{}`), invalid); err == nil {
			t.Fatal("accepted unbounded timeout")
		}
	}
	if len(timeouts) != 2 || timeouts[0] != 0 || timeouts[1] != 3600 {
		t.Fatalf("timeouts=%v", timeouts)
	}
}

func TestStepHonorsAbsoluteDeadlineWithoutExecutingExpiredWork(t *testing.T) {
	expired := time.Now().Add(-time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/poll") {
			_ = json.NewEncoder(w).Encode(map[string]any{"invocation": map[string]any{
				"id": "job", "target_workspace_id": "target", "status": "running",
				"capability": "pi.prompt", "arguments": map[string]any{},
				"timeout_seconds": 3600, "deadline_at": expired,
			}})
			return
		}
		var body struct {
			Error *RemoteError `json:"error"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Error == nil || body.Error.ID != "workspace_execution_failed" {
			t.Error("expired claim was not completed as a safe failure")
		}
		_, _ = w.Write([]byte(`{"invocation":{"id":"job","status":"failed"}}`))
	}))
	defer server.Close()
	client, _ := New(server.URL, "secret")
	called := false
	claimed, err := client.Step(context.Background(), "target", func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		called = true
		return json.RawMessage(`{}`), nil
	})
	if !claimed || err != nil || called {
		t.Fatalf("expired work executed: claimed=%v called=%v err=%v", claimed, called, err)
	}
}

func TestStepLongTaskDeadlineRemainsBounded(t *testing.T) {
	deadline := time.Now().Add(time.Hour)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/poll") {
			_ = json.NewEncoder(w).Encode(map[string]any{"invocation": map[string]any{
				"id": "job", "target_workspace_id": "target", "status": "running",
				"capability": "pi.prompt", "arguments": map[string]any{},
				"timeout_seconds": 7200, "deadline_at": deadline,
			}})
			return
		}
		_, _ = w.Write([]byte(`{"invocation":{"id":"job","status":"succeeded"}}`))
	}))
	defer server.Close()
	client, _ := New(server.URL, "secret")
	_, err := client.Step(context.Background(), "target", func(ctx context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
		got, ok := ctx.Deadline()
		if !ok || !got.Equal(deadline) {
			t.Errorf("deadline not respected: %v", got)
		}
		return json.RawMessage(`{}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
