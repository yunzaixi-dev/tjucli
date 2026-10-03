package workspaceruntime

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func fakeAppendHistory(path, prompt string) int {
	prior, _ := os.ReadFile(path)
	n := bytes.Count(prior, []byte("\n")) + 1
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(3)
	}
	_ = json.NewEncoder(f).Encode(prompt)
	_ = f.Close()
	return n
}

func fakeNativeHistory(runtime string, args []string, prompt string) int {
	if runtime == "pi" {
		for i, arg := range args {
			if arg == "--session" && i+1 < len(args) {
				return fakeAppendHistory(args[i+1], prompt)
			}
		}
	} else {
		for _, arg := range args {
			for _, prefix := range []string{"--session-id=", "--resume="} {
				if id, ok := strings.CutPrefix(arg, prefix); ok {
					return fakeAppendHistory(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), id+".jsonl"), prompt)
				}
			}
		}
	}
	return 0
}

func fakeClaudeStream(record *processRecord, save func(), mode, text string) {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), maxArguments)
	out := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		record.RPC = append(record.RPC, append(json.RawMessage(nil), scanner.Bytes()...))
		save()
		var event struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Message   struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		_ = json.Unmarshal(scanner.Bytes(), &event)
		switch event.Type {
		case "control_request":
			_ = out.Encode(map[string]any{"type": "control_response",
				"response": map[string]any{"subtype": "success", "request_id": event.RequestID, "response": map[string]any{}}})
		case "user":
			record.Input = event.Message.Content
			save()
			n := fakeNativeHistory("claude", record.Args, record.Input)
			if mode == "session-history" {
				text = fmt.Sprintf("turn %d", n)
			}
			if mode == "approval" || mode == "unknown-request" {
				subtype := "can_use_tool"
				if mode == "unknown-request" {
					subtype = "unknown"
				}
				_ = out.Encode(map[string]any{"type": "control_request", "request_id": "tool-approval",
					"request": map[string]any{"subtype": subtype, "tool_name": "Bash",
						"input": map[string]string{"command": "echo fake-secret"}}})
				continue
			}
			_ = out.Encode(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": text})
			return
		case "control_response":
			_ = out.Encode(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": text})
			return
		}
	}
}
