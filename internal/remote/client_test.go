package remote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func createTestTokenFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "token")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatalf("failed to write token file: %v", err)
	}
	return p
}

func TestLoadConfig(t *testing.T) {
	validToken := strings.Repeat("a", 32)
	tokenPath := createTestTokenFile(t, validToken+"\n", 0600)

	t.Run("success loopback http", func(t *testing.T) {
		t.Setenv("TJUCLI_SERVER_URL", "http://127.0.0.1:18090")
		t.Setenv("TJUCLI_TOKEN_FILE", tokenPath)

		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Token != validToken {
			t.Fatalf("expected token %s, got %s", validToken, cfg.Token)
		}
		if cfg.BaseURL.String() != "http://127.0.0.1:18090" {
			t.Fatalf("expected URL http://127.0.0.1:18090, got %s", cfg.BaseURL.String())
		}
	})

	t.Run("success localhost http", func(t *testing.T) {
		t.Setenv("TJUCLI_SERVER_URL", "http://localhost:8080")
		t.Setenv("TJUCLI_TOKEN_FILE", tokenPath)

		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.BaseURL.String() != "http://localhost:8080" {
			t.Fatalf("expected URL http://localhost:8080, got %s", cfg.BaseURL.String())
		}
	})

	t.Run("success remote https", func(t *testing.T) {
		t.Setenv("TJUCLI_SERVER_URL", "https://api.example.com")
		t.Setenv("TJUCLI_TOKEN_FILE", tokenPath)

		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.BaseURL.String() != "https://api.example.com" {
			t.Fatalf("expected URL https://api.example.com, got %s", cfg.BaseURL.String())
		}
	})

	t.Run("missing env vars", func(t *testing.T) {
		t.Setenv("TJUCLI_SERVER_URL", "")
		t.Setenv("TJUCLI_TOKEN_FILE", "")

		_, err := LoadConfig()
		if err == nil || err.Code != "configuration_error" {
			t.Fatalf("expected configuration_error, got %v", err)
		}
	})

	t.Run("reject non-loopback http", func(t *testing.T) {
		t.Setenv("TJUCLI_SERVER_URL", "http://example.com:8080")
		t.Setenv("TJUCLI_TOKEN_FILE", tokenPath)

		_, err := LoadConfig()
		if err == nil || err.Code != "configuration_error" {
			t.Fatalf("expected configuration_error for non-loopback http, got %v", err)
		}
	})

	t.Run("reject URL with path or query", func(t *testing.T) {
		t.Setenv("TJUCLI_SERVER_URL", "https://api.example.com/prefix")
		t.Setenv("TJUCLI_TOKEN_FILE", tokenPath)

		_, err := LoadConfig()
		if err == nil || err.Code != "configuration_error" {
			t.Fatalf("expected configuration_error for path prefix, got %v", err)
		}

		t.Setenv("TJUCLI_SERVER_URL", "https://api.example.com?query=1")
		_, err = LoadConfig()
		if err == nil || err.Code != "configuration_error" {
			t.Fatalf("expected configuration_error for query param, got %v", err)
		}
	})

	t.Run("short token", func(t *testing.T) {
		shortTokenPath := createTestTokenFile(t, "short", 0600)
		t.Setenv("TJUCLI_SERVER_URL", "http://127.0.0.1:18090")
		t.Setenv("TJUCLI_TOKEN_FILE", shortTokenPath)

		_, err := LoadConfig()
		if err == nil || err.Code != "configuration_error" {
			t.Fatalf("expected configuration_error for short token, got %v", err)
		}
	})

	t.Run("token file bad permissions", func(t *testing.T) {
		if isWindows() {
			t.Skip("skipping unix perm test on windows")
		}
		badPermPath := createTestTokenFile(t, validToken, 0644)
		t.Setenv("TJUCLI_SERVER_URL", "http://127.0.0.1:18090")
		t.Setenv("TJUCLI_TOKEN_FILE", badPermPath)

		_, err := LoadConfig()
		if err == nil || err.Code != "configuration_error" {
			t.Fatalf("expected configuration_error for 0644 token file, got %v", err)
		}
	})
}

func TestClient_ListAndSearch(t *testing.T) {
	expectedToken := "test-secret-token-123456789012345678"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+expectedToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"ok":false,"error":{"code":"unauthorized","message":"missing bearer token"}}`))
			return
		}

		switch r.URL.Path {
		case "/v1/course/list":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(`{"ok":true,"data":{"items":[{"name":"test.pdf","path":"/test.pdf","kind":"file","size":123,"modified_at":"2026-09-09T00:00:00Z"}]},"meta":{"next_cursor":"cursor1"}}`))
		case "/v1/course/search":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(`{"ok":true,"data":{"items":[{"name":"matched.pdf","path":"/matched.pdf","kind":"file","size":456,"modified_at":"2026-09-09T00:00:00Z"}]},"meta":{"scope":"course-catalog","pages_scanned":1,"incomplete":false}}`))
		case "/v1/knowledge/search":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(`{"ok":true,"data":{"hits":[{"source":"course","item_id":"item-1","source_url":"https://example.com/doc","canonical_content_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","knowledge_id":"knowledge-1","chunk_id":"chunk-1","quoted_text":"quoted","score":0.9}]},"meta":{}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	client := NewClient(Config{
		BaseURL: u,
		Token:   expectedToken,
	})

	t.Run("List success", func(t *testing.T) {
		res, meta, cliErr := client.List(context.Background(), "/", "")
		if cliErr != nil {
			t.Fatalf("unexpected List error: %v", cliErr)
		}
		if len(res.Items) != 1 || res.Items[0].Name != "test.pdf" {
			t.Fatalf("unexpected items: %+v", res.Items)
		}
		if meta.NextCursor == nil || *meta.NextCursor != "cursor1" {
			t.Fatalf("unexpected cursor: %v", meta.NextCursor)
		}
	})

	t.Run("Search success", func(t *testing.T) {
		res, meta, cliErr := client.Search(context.Background(), "query", 5, 10)
		if cliErr != nil {
			t.Fatalf("unexpected Search error: %v", cliErr)
		}
		if len(res.Items) != 1 || res.Items[0].Name != "matched.pdf" {
			t.Fatalf("unexpected search items: %+v", res.Items)
		}
		if meta.PagesScanned != 1 || meta.Incomplete {
			t.Fatalf("unexpected meta: %+v", meta)
		}
	})

	t.Run("KnowledgeSearch success", func(t *testing.T) {
		res, cliErr := client.KnowledgeSearch(context.Background(), "query", 1, "course")
		if cliErr != nil || len(res.Hits) != 1 || res.Hits[0].QuotedText != "quoted" {
			t.Fatalf("unexpected KnowledgeSearch result: %+v, %v", res, cliErr)
		}
	})
}

func TestClient_KnowledgeSearch_DoesNotFallback(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.URL.Path != "/v1/knowledge/search" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"ok":false,"error":{"code":"upstream_error","message":"hidden"}}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	client := NewClient(Config{BaseURL: u, Token: strings.Repeat("a", 32)})
	_, cliErr := client.KnowledgeSearch(context.Background(), "query", 1, "course")
	if !called || cliErr == nil || cliErr.Code != "upstream_error" {
		t.Fatalf("expected fixed-route upstream error, called=%t err=%v", called, cliErr)
	}
}

func TestClient_RedirectRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://attacker.com/steal", http.StatusFound)
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	client := NewClient(Config{
		BaseURL: u,
		Token:   strings.Repeat("a", 32),
	})

	_, _, err := client.List(context.Background(), "/", "")
	if err == nil || err.Code != "protocol_error" || !strings.Contains(err.Message, "redirect") {
		t.Fatalf("expected redirect protocol_error, got %v", err)
	}
}

func TestClient_Download_SuccessAndConfinement(t *testing.T) {
	content := []byte("Hello remote course download!")
	h := sha256.Sum256(content)
	contentSHA := hex.EncodeToString(h[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/course/download" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.Header().Set("X-Content-SHA256", contentSHA)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	client := NewClient(Config{
		BaseURL: u,
		Token:   strings.Repeat("a", 32),
	})

	workDir := t.TempDir()
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWd) }()

	t.Run("successful download", func(t *testing.T) {
		targetFile := "downloaded.txt"
		res, cliErr := client.Download(context.Background(), "/file.txt", targetFile, 1024*1024)
		if cliErr != nil {
			t.Fatalf("unexpected download error: %v", cliErr)
		}
		if res.Bytes != int64(len(content)) {
			t.Fatalf("expected %d bytes, got %d", len(content), res.Bytes)
		}
		if res.SHA256 != contentSHA {
			t.Fatalf("expected sha %s, got %s", contentSHA, res.SHA256)
		}
		data, err := os.ReadFile(filepath.Join(workDir, targetFile))
		if err != nil {
			t.Fatalf("failed to read downloaded file: %v", err)
		}
		if string(data) != string(content) {
			t.Fatalf("content mismatch: got %q", string(data))
		}
	})

	t.Run("reject existing target file", func(t *testing.T) {
		targetFile := "existing.txt"
		if err := os.WriteFile(filepath.Join(workDir, targetFile), []byte("already here"), 0644); err != nil {
			t.Fatal(err)
		}

		_, cliErr := client.Download(context.Background(), "/file.txt", targetFile, 1024*1024)
		if cliErr == nil || cliErr.Code != "target_exists" {
			t.Fatalf("expected target_exists error, got %v", cliErr)
		}
	})

	t.Run("reject traversal path escaping workspace", func(t *testing.T) {
		targetFile := "../escape.txt"
		_, cliErr := client.Download(context.Background(), "/file.txt", targetFile, 1024*1024)
		if cliErr == nil || cliErr.Code != "target_outside_workspace" {
			t.Fatalf("expected target_outside_workspace, got %v", cliErr)
		}
	})

	t.Run("accept dot-dot-prefixed filename within workspace", func(t *testing.T) {
		// A filename like "..file.txt" inside workDir is not a directory traversal.
		targetFile := filepath.Join(workDir, "..file.txt")
		res, cliErr := client.Download(context.Background(), "/file.txt", targetFile, 1024*1024)
		if cliErr != nil {
			t.Fatalf("unexpected error for dot-dot prefixed filename: %v", cliErr)
		}
		if res.Bytes != int64(len(content)) {
			t.Fatalf("expected %d bytes, got %d", len(content), res.Bytes)
		}
	})
}

func TestClient_Download_ChecksumMismatch(t *testing.T) {
	content := []byte("content for mismatch test")
	wrongSHA := strings.Repeat("0", 64)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.Header().Set("X-Content-SHA256", wrongSHA)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	client := NewClient(Config{
		BaseURL: u,
		Token:   strings.Repeat("a", 32),
	})

	workDir := t.TempDir()
	origWd, _ := os.Getwd()
	_ = os.Chdir(workDir)
	defer func() { _ = os.Chdir(origWd) }()

	target := "mismatch.txt"
	_, cliErr := client.Download(context.Background(), "/file.txt", target, 1024*1024)
	if cliErr == nil || cliErr.Code != "protocol_error" {
		t.Fatalf("expected protocol_error for checksum mismatch, got %v", cliErr)
	}
	if _, err := os.Stat(filepath.Join(workDir, target)); err == nil {
		t.Fatal("file should not exist after checksum failure")
	}
}

func TestClient_Download_CancellationAndNoRename(t *testing.T) {
	content := []byte("content for cancel test")
	h := sha256.Sum256(content)
	contentSHA := hex.EncodeToString(h[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.Header().Set("X-Content-SHA256", contentSHA)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	client := NewClient(Config{
		BaseURL: u,
		Token:   strings.Repeat("a", 32),
	})

	workDir := t.TempDir()
	origWd, _ := os.Getwd()
	_ = os.Chdir(workDir)
	defer func() { _ = os.Chdir(origWd) }()

	// Test context cancelled before publication
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	target := "cancelled.txt"
	_, cliErr := client.Download(ctx, "/file.txt", target, 1024*1024)
	if cliErr == nil {
		t.Fatal("expected error on cancelled context")
	}
	if _, err := os.Stat(filepath.Join(workDir, target)); err == nil {
		t.Fatal("target file must not be published on cancelled context")
	}

	// Verify temp directory has 0700 permissions and temp files are cleaned up
	entries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tjucli-dl-") {
			t.Fatalf("temp file was not cleaned up: %s", e.Name())
		}
	}
}

func TestClient_SafeErrorRendering(t *testing.T) {
	// Verify that sensitive details in error body (tokens, internal paths, upstream URLs) are NEVER reflected
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ok":false,"error":{"code":"leak_secret","message":"token=SUPER_SECRET_123 path=/etc/shadow https://internal.corp/admin"}}`))
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	client := NewClient(Config{
		BaseURL: u,
		Token:   strings.Repeat("a", 32),
	})

	_, _, cliErr := client.List(context.Background(), "/", "")
	if cliErr == nil {
		t.Fatal("expected error")
	}
	msg := cliErr.Error()
	for _, sensitive := range []string{"SUPER_SECRET_123", "/etc/shadow", "https://internal.corp", "leak_secret"} {
		if strings.Contains(msg, sensitive) {
			t.Fatalf("error reflected sensitive payload %q: %s", sensitive, msg)
		}
	}
	if cliErr.Code != "upstream_error" {
		t.Fatalf("expected mapped upstream_error code, got %s", cliErr.Code)
	}
}

func TestClient_SchemaValidation(t *testing.T) {
	t.Run("reject missing items in data", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(`{"ok":true,"data":{},"meta":{}}`))
		}))
		defer srv.Close()

		u, _ := url.Parse(srv.URL)
		client := NewClient(Config{BaseURL: u, Token: strings.Repeat("a", 32)})

		_, _, err := client.List(context.Background(), "/", "")
		if err == nil || err.Code != "protocol_error" {
			t.Fatalf("expected protocol_error for missing items, got %v", err)
		}
	})

	t.Run("reject null item field", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(`{"ok":true,"data":{"items":[{"name":"test","path":null,"kind":"file","size":10,"modified_at":"2026-09-09T00:00:00Z"}]},"meta":{}}`))
		}))
		defer srv.Close()

		u, _ := url.Parse(srv.URL)
		client := NewClient(Config{BaseURL: u, Token: strings.Repeat("a", 32)})

		_, _, err := client.List(context.Background(), "/", "")
		if err == nil || err.Code != "protocol_error" {
			t.Fatalf("expected protocol_error for null field in item, got %v", err)
		}
	})

	t.Run("reject invalid item kind", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(`{"ok":true,"data":{"items":[{"name":"test","path":"/test","kind":"symlink","size":10,"modified_at":"2026-09-09T00:00:00Z"}]},"meta":{}}`))
		}))
		defer srv.Close()

		u, _ := url.Parse(srv.URL)
		client := NewClient(Config{BaseURL: u, Token: strings.Repeat("a", 32)})

		_, _, err := client.List(context.Background(), "/", "")
		if err == nil || err.Code != "protocol_error" {
			t.Fatalf("expected protocol_error for invalid kind, got %v", err)
		}
	})

	t.Run("reject missing search meta fields", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(`{"ok":true,"data":{"items":[]},"meta":{"scope":"test"}}`))
		}))
		defer srv.Close()

		u, _ := url.Parse(srv.URL)
		client := NewClient(Config{BaseURL: u, Token: strings.Repeat("a", 32)})

		_, _, err := client.Search(context.Background(), "query", 5, 10)
		if err == nil || err.Code != "protocol_error" {
			t.Fatalf("expected protocol_error for missing search meta, got %v", err)
		}
	})
}

func TestClient_ExactMIMEValidation(t *testing.T) {
	t.Run("reject text/plain for JSON endpoint", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(`{"ok":true,"data":{"items":[]},"meta":{}}`))
		}))
		defer srv.Close()

		u, _ := url.Parse(srv.URL)
		client := NewClient(Config{BaseURL: u, Token: strings.Repeat("a", 32)})

		_, _, err := client.List(context.Background(), "/", "")
		if err == nil || err.Code != "protocol_error" {
			t.Fatalf("expected protocol_error for text/plain MIME, got %v", err)
		}
	})

	t.Run("reject application/json for download endpoint", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "10")
			w.Header().Set("X-Content-SHA256", strings.Repeat("a", 64))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("1234567890"))
		}))
		defer srv.Close()

		u, _ := url.Parse(srv.URL)
		client := NewClient(Config{BaseURL: u, Token: strings.Repeat("a", 32)})

		workDir := t.TempDir()
		origWd, _ := os.Getwd()
		_ = os.Chdir(workDir)
		defer func() { _ = os.Chdir(origWd) }()

		_, err := client.Download(context.Background(), "/file.txt", "out.txt", 1024)
		if err == nil || err.Code != "protocol_error" {
			t.Fatalf("expected protocol_error for non-octet-stream download MIME, got %v", err)
		}
	})
}
