package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestToolsListAndCallThroughTurnProxy(t *testing.T) {
	var calls []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer turn-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+r.URL.Path+" "+string(body))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"tools":[{"type":"function","function":{"name":"read_entry","description":"读取笔记","parameters":{"type":"object"}}}]}`)
		case strings.Contains(string(body), `"missing"`):
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"id":"unknown_tool"}}`)
		default:
			_, _ = io.WriteString(w, `{"name":"read_entry","result":"{\"title\":\"电路\"}"}`)
		}
	}))
	defer proxy.Close()
	t.Setenv("TJUCLI_TOOLS_URL", proxy.URL)
	t.Setenv("TJUCLI_TOOLS_TOKEN", "turn-token")
	run := func(args ...string) (int, map[string]any) {
		var out bytes.Buffer
		code := (&runner{stdout: &out, stderr: io.Discard}).run(context.Background(), args)
		var envelope map[string]any
		_ = json.Unmarshal(out.Bytes(), &envelope)
		return code, envelope
	}
	code, listed := run("tools", "list", "--json")
	if code != 0 || !strings.Contains(mustJSON(listed), `"name":"read_entry"`) {
		t.Fatalf("list: %d %v", code, listed)
	}
	code, called := run("tools", "call", "read_entry", "--args", `{"id":"abc"}`, "--json")
	if code != 0 || !strings.Contains(mustJSON(called), `"result":{"title":"电路"}`) {
		t.Fatalf("call: %d %v", code, called)
	}
	if want := `POST /v1/tools/call {"arguments":{"id":"abc"},"name":"read_entry"}`; calls[1] != want {
		t.Fatalf("call body %q", calls[1])
	}
	if code, failed := run("tools", "call", "missing", "--json"); code != 1 || !strings.Contains(mustJSON(failed), "unknown_tool") {
		t.Fatalf("unknown: %d %v", code, failed)
	}
	if code, _ := run("tools", "call", "read_entry", "--args", `[1]`, "--json"); code != 2 {
		t.Fatalf("non-object args accepted: %d", code)
	}
	t.Setenv("TJUCLI_TOOLS_URL", "http://example.com")
	if code, failed := run("tools", "list", "--json"); code != 1 || !strings.Contains(mustJSON(failed), "configuration_error") {
		t.Fatalf("non-loopback HTTP accepted: %d %v", code, failed)
	}
	t.Setenv("TJUCLI_TOOLS_URL", "")
	if code, failed := run("tools", "list", "--json"); code != 1 || !strings.Contains(mustJSON(failed), "tools_unavailable") {
		t.Fatalf("outside a turn: %d %v", code, failed)
	}
}

func mustJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}
