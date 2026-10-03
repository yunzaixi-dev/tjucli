package workspacebridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestStepDurablePreflightsStorageBeforeClaimOrExecute(t *testing.T) {
	var requests, executions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"invocation":{"id":"one","target_workspace_id":"local","status":"running","capability":"pi.prompt","arguments":{"prompt":"must not execute"}}}`))
	}))
	defer server.Close()
	client, err := New(server.URL, "connection-token")
	if err != nil {
		t.Fatal(err)
	}
	unsafe := &CompletionOutbox{dir: filepath.Join(t.TempDir(), "missing")}
	did, err := client.StepDurable(context.Background(), "local", func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		executions.Add(1)
		return json.RawMessage(`{"output":"unexpected"}`), nil
	}, unsafe)
	if err == nil || did || requests.Load() != 0 || executions.Load() != 0 {
		t.Fatal("unsafe completion storage allowed a claim or execution")
	}
}
