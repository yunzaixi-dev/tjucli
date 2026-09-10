package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yunzaixi-dev/tjucli/internal/tjucli"
)

func TestRemoteE2E_Commands(t *testing.T) {
	validToken := strings.Repeat("b", 32)
	tmpDir := t.TempDir()
	tokenFile := filepath.Join(tmpDir, "token")
	if err := os.WriteFile(tokenFile, []byte(validToken), 0600); err != nil {
		t.Fatal(err)
	}

	dlContent := []byte("remote e2e downloaded data")
	h := sha256.Sum256(dlContent)
	dlSHA := hex.EncodeToString(h[:])

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+validToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"ok":false,"error":{"code":"unauthorized","message":"missing bearer token"}}`))
			return
		}

		switch r.URL.Path {
		case "/v1/course/list":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(`{"ok":true,"data":{"items":[{"name":"file.txt","path":"/file.txt","kind":"file","size":26,"modified_at":"2026-09-09T00:00:00Z"}]},"meta":{"next_cursor":null}}`))
		case "/v1/course/search":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(`{"ok":true,"data":{"items":[{"name":"file.txt","path":"/file.txt","kind":"file","size":26,"modified_at":"2026-09-09T00:00:00Z"}]},"meta":{"scope":"course-catalog","pages_scanned":1,"incomplete":false}}`))
		case "/v1/course/download":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(dlContent)))
			w.Header().Set("X-Content-SHA256", dlSHA)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(dlContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("TJUCLI_MODE", "remote")
	t.Setenv("TJUCLI_SERVER_URL", server.URL)
	t.Setenv("TJUCLI_TOKEN_FILE", tokenFile)

	origWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(origWd) }()

	t.Run("course ls --json", func(t *testing.T) {
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		code := (&runner{stdout: stdout, stderr: stderr}).run(context.Background(), []string{"course", "ls", "--json"})
		if code != 0 {
			t.Fatalf("exit code %d, stderr: %s", code, stderr.String())
		}
		var env tjucli.SuccessEnvelope
		if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if !env.OK {
			t.Fatalf("expected OK true, got %#v", env)
		}
	})

	t.Run("course search --json", func(t *testing.T) {
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		code := (&runner{stdout: stdout, stderr: stderr}).run(context.Background(), []string{"course", "search", "file", "--json"})
		if code != 0 {
			t.Fatalf("exit code %d, stderr: %s", code, stderr.String())
		}
		var env tjucli.SuccessEnvelope
		if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if !env.OK {
			t.Fatalf("expected OK true, got %#v", env)
		}
	})

	t.Run("course download --json", func(t *testing.T) {
		targetFile := "downloaded-file.txt"
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		code := (&runner{stdout: stdout, stderr: stderr}).run(context.Background(), []string{"course", "download", "/file.txt", "--output", targetFile, "--json"})
		if code != 0 {
			t.Fatalf("exit code %d, stderr: %s", code, stderr.String())
		}
		var env tjucli.SuccessEnvelope
		if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if !env.OK {
			t.Fatalf("expected OK true, got %#v", env)
		}

		data, err := os.ReadFile(filepath.Join(tmpDir, targetFile))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != string(dlContent) {
			t.Fatalf("content mismatch: %s vs %s", string(data), string(dlContent))
		}
	})
}
