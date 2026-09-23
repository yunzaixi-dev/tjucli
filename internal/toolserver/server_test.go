package toolserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yunzaixi-dev/tjucli/internal/knowledge"
	"github.com/yunzaixi-dev/tjucli/internal/tjucli"
)

type mockProvider struct {
	listFunc     func(ctx context.Context, providerPath, cursor string) (tjucli.ListResult, tjucli.ListMeta, *tjucli.CLIError)
	searchFunc   func(ctx context.Context, query string, maxPages, limit int) (tjucli.SearchResult, tjucli.SearchMeta, *tjucli.CLIError)
	downloadFunc func(ctx context.Context, providerPath, outputPath string, maxBytes int64) (tjucli.DownloadResult, *tjucli.CLIError)
}

func (m *mockProvider) List(ctx context.Context, providerPath, cursor string) (tjucli.ListResult, tjucli.ListMeta, *tjucli.CLIError) {
	if m.listFunc != nil {
		return m.listFunc(ctx, providerPath, cursor)
	}
	return tjucli.ListResult{}, tjucli.ListMeta{}, nil
}

func (m *mockProvider) Search(ctx context.Context, query string, maxPages, limit int) (tjucli.SearchResult, tjucli.SearchMeta, *tjucli.CLIError) {
	if m.searchFunc != nil {
		return m.searchFunc(ctx, query, maxPages, limit)
	}
	return tjucli.SearchResult{}, tjucli.SearchMeta{}, nil
}

func (m *mockProvider) Download(ctx context.Context, providerPath, outputPath string, maxBytes int64) (tjucli.DownloadResult, *tjucli.CLIError) {
	if m.downloadFunc != nil {
		return m.downloadFunc(ctx, providerPath, outputPath, maxBytes)
	}
	return tjucli.DownloadResult{}, nil
}

type mockAuthorizer struct {
	authFunc func(ctx context.Context, token, requiredScope string) (*Grant, *AuthError)
}

type mockKnowledgeProvider struct {
	searchFunc func(context.Context, string, int, string) (knowledge.SearchResult, *tjucli.CLIError)
}

func (m *mockKnowledgeProvider) Search(ctx context.Context, query string, limit int, source string) (knowledge.SearchResult, *tjucli.CLIError) {
	if m.searchFunc != nil {
		return m.searchFunc(ctx, query, limit, source)
	}
	return knowledge.SearchResult{}, nil
}

func (m *mockAuthorizer) Authorize(ctx context.Context, token, requiredScope string) (*Grant, *AuthError) {
	if m.authFunc != nil {
		return m.authFunc(ctx, token, requiredScope)
	}
	return nil, &AuthError{StatusCode: http.StatusUnauthorized, Code: "unauthorized", Message: "unauthorized"}
}

func setupTestServer(t *testing.T, mp *mockProvider) (*Server, string, string) {
	t.Helper()
	token := "secret-test-token"
	tokenHex := hashToken(token)
	expiry := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	grantsJSON := fmt.Sprintf(`{"grants":[{"token_sha256":"%s","run_id":"0123456789abcdef0123456789abcdef","expires_at":"%s","scopes":["course:read","knowledge:read"]}]}`, tokenHex, expiry)

	grantsPath := createTestGrantsFile(t, grantsJSON, 0600)
	authorizer := NewFileAuthorizer(grantsPath)

	srv, err := NewServer(ServerConfig{
		Addr:           "127.0.0.1:0",
		Authorizer:     authorizer,
		CourseProvider: mp,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	t.Cleanup(func() {
		_ = srv.Close()
	})

	return srv, token, grantsPath
}

func TestServer_Healthz(t *testing.T) {
	srv, _, _ := setupTestServer(t, &mockProvider{})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("expected no-store, got %s", w.Header().Get("Cache-Control"))
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("expected nosniff, got %s", w.Header().Get("X-Content-Type-Options"))
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != true {
		t.Fatalf("expected ok:true, got %#v", resp)
	}
}

func TestServer_Healthz_MethodNotAllowed(t *testing.T) {
	srv, _, _ := setupTestServer(t, &mockProvider{})

	req := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestServer_AuthAndMethods(t *testing.T) {
	srv, token, _ := setupTestServer(t, &mockProvider{})

	routes := []string{"/v1/course/list", "/v1/course/search", "/v1/course/download", "/v1/knowledge/search"}

	for _, route := range routes {
		t.Run(route+" GET rejected 405", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, route, nil)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("expected 405, got %d", w.Code)
			}
		})

		t.Run(route+" no auth 401", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, route, bytes.NewReader([]byte(`{}`)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d", w.Code)
			}
		})

		t.Run(route+" invalid auth 401", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, route, bytes.NewReader([]byte(`{}`)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer invalid-token")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d", w.Code)
			}
		})

		t.Run(route+" missing content-type 415", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, route, bytes.NewReader([]byte(`{}`)))
			req.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("expected 415, got %d", w.Code)
			}
		})

		t.Run(route+" content-type suffix rejected 415", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, route, bytes.NewReader([]byte(`{}`)))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json-evil")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("expected 415 for application/json-evil, got %d", w.Code)
			}
		})

		t.Run(route+" unknown json field 400 without reflection", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, route, bytes.NewReader([]byte(`{"unknown_secret_param":"confidential_value"}`)))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "confidential_value") {
				t.Fatalf("unknown field value reflected in error: %s", w.Body.String())
			}
		})

		t.Run(route+" trailing json 400", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, route, bytes.NewReader([]byte(`{}{} `)))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
		})

		t.Run(route+" body too large 413", func(t *testing.T) {
			largeData := `{"path":"` + strings.Repeat("x", int(MaxRequestBodyBytes)+10) + `"}`
			req := httptest.NewRequest(http.MethodPost, route, bytes.NewReader([]byte(largeData)))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("expected 413, got %d", w.Code)
			}
		})
	}
}

func TestServer_ExpiredGrantRejected(t *testing.T) {
	// Authorizer returns an already expired grant
	mockAuth := &mockAuthorizer{
		authFunc: func(ctx context.Context, token, requiredScope string) (*Grant, *AuthError) {
			return &Grant{
				TokenSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				RunID:       "0123456789abcdef0123456789abcdef",
				ExpiresAt:   time.Now().Add(-10 * time.Minute).Format(time.RFC3339),
				Scopes:      []string{"course:read"},
			}, nil
		},
	}

	srv, err := NewServer(ServerConfig{
		Addr:           "127.0.0.1:0",
		Authorizer:     mockAuth,
		CourseProvider: &mockProvider{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/course/list", bytes.NewReader([]byte(`{"path":"/"}`)))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for expired grant, got %d", w.Code)
	}
}

func TestServer_KnowledgeSearch_ExpiredGrantRejected(t *testing.T) {
	mockAuth := &mockAuthorizer{authFunc: func(context.Context, string, string) (*Grant, *AuthError) {
		return &Grant{ExpiresAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), Scopes: []string{RequiredScopeKnowledgeRead}}, nil
	}}
	srv, err := NewServer(ServerConfig{Addr: "127.0.0.1:0", Authorizer: mockAuth, KnowledgeProvider: &mockKnowledgeProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/knowledge/search", strings.NewReader(`{"query":"q"}`))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for expired knowledge grant, got %d", w.Code)
	}
}

func TestServer_SpoolBaseDir_NeverRemovesParent(t *testing.T) {
	baseDir := t.TempDir()
	canaryFile := filepath.Join(baseDir, "canary.txt")
	if err := os.WriteFile(canaryFile, []byte("canary"), 0600); err != nil {
		t.Fatal(err)
	}

	token := "token"
	tokenHex := hashToken(token)
	expiry := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	grantsJSON := fmt.Sprintf(`{"grants":[{"token_sha256":"%s","run_id":"0123456789abcdef0123456789abcdef","expires_at":"%s","scopes":["course:read"]}]}`, tokenHex, expiry)
	grantsPath := createTestGrantsFile(t, grantsJSON, 0600)

	srv, err := NewServer(ServerConfig{
		Addr:           "127.0.0.1:0",
		SpoolBaseDir:   baseDir,
		Authorizer:     NewFileAuthorizer(grantsPath),
		CourseProvider: &mockProvider{},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Close server and ensure baseDir and canaryFile still exist
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(canaryFile); err != nil {
		t.Fatalf("canary file in SpoolBaseDir was removed: %v", err)
	}
}

func TestServer_CourseList_Success(t *testing.T) {
	nextCursor := "cur-2"
	mp := &mockProvider{
		listFunc: func(ctx context.Context, providerPath, cursor string) (tjucli.ListResult, tjucli.ListMeta, *tjucli.CLIError) {
			if providerPath != "/courses" || cursor != "cur-1" {
				return tjucli.ListResult{}, tjucli.ListMeta{}, &tjucli.CLIError{Code: "invalid_argument", Message: "bad path/cursor"}
			}
			return tjucli.ListResult{
				Items: []tjucli.Item{
					{Name: "Item 1", Path: "/courses/1", Kind: tjucli.KindFile, Size: 100},
				},
			}, tjucli.ListMeta{NextCursor: &nextCursor}, nil
		},
	}
	srv, token, _ := setupTestServer(t, mp)

	body := `{"path":"/courses","cursor":"cur-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/course/list", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("expected no-store, got %s", w.Header().Get("Cache-Control"))
	}
	var resp tjucli.SuccessEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("expected ok:true, got false")
	}
}

func TestServer_KnowledgeSearch_SuccessPreservesCitations(t *testing.T) {
	const token = "knowledge-token"
	hit := knowledge.Hit{Source: "course", ItemID: "item-1", SourceURL: "https://example.com/doc", CanonicalContentHash: strings.Repeat("a", 64), KnowledgeID: "knowledge-1", ChunkID: "chunk-1", QuotedText: "quoted", Score: 0.9}
	provider := &mockKnowledgeProvider{searchFunc: func(ctx context.Context, query string, limit int, source string) (knowledge.SearchResult, *tjucli.CLIError) {
		if query != "calculus" || limit != 4 || source != "course" {
			return knowledge.SearchResult{}, tjucli.NewFlagError("unexpected knowledge arguments")
		}
		return knowledge.SearchResult{Hits: []knowledge.Hit{hit}}, nil
	}}
	grant := &Grant{ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), Scopes: []string{RequiredScopeKnowledgeRead}}
	srv, err := NewServer(ServerConfig{Addr: "127.0.0.1:0", Authorizer: &mockAuthorizer{authFunc: func(context.Context, string, string) (*Grant, *AuthError) { return grant, nil }}, KnowledgeProvider: provider})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/knowledge/search", strings.NewReader(`{"query":"calculus","limit":4,"source":"course"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), hit.QuotedText) || !strings.Contains(w.Body.String(), hit.SourceURL) {
		t.Fatalf("unexpected knowledge response: %d %s", w.Code, w.Body.String())
	}
}

func TestServer_KnowledgeSearch_RequiresDedicatedScope(t *testing.T) {
	srv, err := NewServer(ServerConfig{Addr: "127.0.0.1:0", Authorizer: &mockAuthorizer{authFunc: func(_ context.Context, _, scope string) (*Grant, *AuthError) {
		if scope != RequiredScopeKnowledgeRead {
			return nil, &AuthError{StatusCode: http.StatusForbidden, Code: "forbidden", Message: "forbidden"}
		}
		return nil, &AuthError{StatusCode: http.StatusForbidden, Code: "forbidden", Message: "forbidden"}
	}}, KnowledgeProvider: &mockKnowledgeProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/knowledge/search", strings.NewReader(`{"query":"q"}`))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestServer_KnowledgeSearch_RejectsMalformedRequestAndProviderFailure(t *testing.T) {
	for _, test := range []struct {
		name        string
		body        string
		providerErr *tjucli.CLIError
		status      int
		code        string
	}{
		{name: "malformed", body: `{"query":"q","unknown":true}`, status: http.StatusBadRequest, code: "invalid_argument"},
		{name: "upstream", body: `{"query":"q"}`, providerErr: tjucli.NewRuntimeError("upstream_error", "secret upstream detail"), status: http.StatusBadGateway, code: "upstream_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := &mockKnowledgeProvider{searchFunc: func(context.Context, string, int, string) (knowledge.SearchResult, *tjucli.CLIError) {
				return knowledge.SearchResult{}, test.providerErr
			}}
			grant := &Grant{ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), Scopes: []string{RequiredScopeKnowledgeRead}}
			srv, err := NewServer(ServerConfig{Addr: "127.0.0.1:0", Authorizer: &mockAuthorizer{authFunc: func(context.Context, string, string) (*Grant, *AuthError) { return grant, nil }}, KnowledgeProvider: provider})
			requireNoError(t, err)
			defer srv.Close()
			req := httptest.NewRequest(http.MethodPost, "/v1/knowledge/search", strings.NewReader(test.body))
			req.Header.Set("Authorization", "Bearer token")
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != test.status || !strings.Contains(w.Body.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("expected %d/%s, got %d: %s", test.status, test.code, w.Code, w.Body.String())
			}
		})
	}
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestServer_CourseSearch_Success(t *testing.T) {
	mp := &mockProvider{
		searchFunc: func(ctx context.Context, query string, maxPages, limit int) (tjucli.SearchResult, tjucli.SearchMeta, *tjucli.CLIError) {
			if query != "math" || maxPages != 5 || limit != 10 {
				return tjucli.SearchResult{}, tjucli.SearchMeta{}, &tjucli.CLIError{Code: "invalid_argument", Message: "mismatch args"}
			}
			return tjucli.SearchResult{
				Items: []tjucli.Item{
					{Name: "Math 101", Path: "/courses/math", Kind: tjucli.KindFile},
				},
			}, tjucli.SearchMeta{Scope: "all", PagesScanned: 2, Incomplete: false}, nil
		},
	}
	srv, token, _ := setupTestServer(t, mp)

	body := `{"query":"math","max_pages":5,"limit":10}`
	req := httptest.NewRequest(http.MethodPost, "/v1/course/search", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestServer_CourseDownload_Success(t *testing.T) {
	fileContent := []byte("hello world tjucli download content")
	expectedSHA := sha256.Sum256(fileContent)
	expectedSHAHex := hex.EncodeToString(expectedSHA[:])

	mp := &mockProvider{
		downloadFunc: func(ctx context.Context, providerPath, outputPath string, maxBytes int64) (tjucli.DownloadResult, *tjucli.CLIError) {
			if providerPath != "/course/doc.pdf" {
				return tjucli.DownloadResult{}, &tjucli.CLIError{Code: "not_found", Message: "not found"}
			}
			if err := os.WriteFile(outputPath, fileContent, 0600); err != nil {
				return tjucli.DownloadResult{}, &tjucli.CLIError{Code: "internal_error", Message: err.Error()}
			}
			return tjucli.DownloadResult{
				LocalPath: outputPath,
				Bytes:     int64(len(fileContent)),
				SHA256:    expectedSHAHex,
			}, nil
		},
	}
	srv, token, _ := setupTestServer(t, mp)

	body := `{"path":"/course/doc.pdf"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/course/download", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("expected no-store, got %s", w.Header().Get("Cache-Control"))
	}
	if w.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("expected application/octet-stream, got %s", w.Header().Get("Content-Type"))
	}
	if w.Header().Get("Content-Length") != fmt.Sprintf("%d", len(fileContent)) {
		t.Fatalf("expected Content-Length %d, got %s", len(fileContent), w.Header().Get("Content-Length"))
	}
	if w.Header().Get("X-Content-SHA256") != expectedSHAHex {
		t.Fatalf("expected X-Content-SHA256 %s, got %s", expectedSHAHex, w.Header().Get("X-Content-SHA256"))
	}

	if !bytes.Equal(w.Body.Bytes(), fileContent) {
		t.Fatalf("body mismatch: got %q, want %q", w.Body.String(), string(fileContent))
	}
}

func TestServer_CourseDownload_ExceedsMaxBytes(t *testing.T) {
	srv, token, _ := setupTestServer(t, &mockProvider{})

	body := fmt.Sprintf(`{"path":"/file","max_bytes":%d}`, MaxDownloadBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/v1/course/download", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestServer_ProviderErrorRedaction(t *testing.T) {
	mp := &mockProvider{
		listFunc: func(ctx context.Context, providerPath, cursor string) (tjucli.ListResult, tjucli.ListMeta, *tjucli.CLIError) {
			return tjucli.ListResult{}, tjucli.ListMeta{}, &tjucli.CLIError{
				Code:    "network_failure",
				Message: "failed connecting to https://secret-internal-provider.tju.edu.cn/api?token=secret123",
			}
		},
	}
	srv, token, _ := setupTestServer(t, mp)

	body := `{"path":"/"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/course/list", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", w.Code)
	}
	respStr := w.Body.String()
	if strings.Contains(respStr, "secret") || strings.Contains(respStr, "token") {
		t.Fatalf("sensitive details leaked in error response: %s", respStr)
	}
	var env tjucli.FailureEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "upstream_error" {
		t.Fatalf("expected upstream_error, got %s", env.Error.Code)
	}
}

func TestServer_KnowledgeSearch_RejectsMalformedProviderResult(t *testing.T) {
	grant := &Grant{ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), Scopes: []string{RequiredScopeKnowledgeRead}}
	srv, err := NewServer(ServerConfig{
		Addr:       "127.0.0.1:0",
		Authorizer: &mockAuthorizer{authFunc: func(context.Context, string, string) (*Grant, *AuthError) { return grant, nil }},
		KnowledgeProvider: &mockKnowledgeProvider{searchFunc: func(context.Context, string, int, string) (knowledge.SearchResult, *tjucli.CLIError) {
			return knowledge.SearchResult{Hits: []knowledge.Hit{{Source: "missing-citation"}}}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/knowledge/search", strings.NewReader(`{"query":"q"}`))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), `"code":"protocol_error"`) {
		t.Fatalf("expected protocol error, got %d: %s", w.Code, w.Body.String())
	}
}

func TestServer_ConcurrencyLimiting(t *testing.T) {
	blocked := make(chan struct{})
	enteredCount := 0
	var mu sync.Mutex

	mp := &mockProvider{
		listFunc: func(ctx context.Context, providerPath, cursor string) (tjucli.ListResult, tjucli.ListMeta, *tjucli.CLIError) {
			mu.Lock()
			enteredCount++
			mu.Unlock()
			<-blocked
			return tjucli.ListResult{}, tjucli.ListMeta{}, nil
		},
	}

	token := "secret-test-token"
	tokenHex := hashToken(token)
	expiry := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	grantsJSON := fmt.Sprintf(`{"grants":[{"token_sha256":"%s","run_id":"0123456789abcdef0123456789abcdef","expires_at":"%s","scopes":["course:read"]}]}`, tokenHex, expiry)
	grantsPath := createTestGrantsFile(t, grantsJSON, 0600)
	authorizer := NewFileAuthorizer(grantsPath)

	srv, err := NewServer(ServerConfig{
		Addr:           "127.0.0.1:0",
		Authorizer:     authorizer,
		CourseProvider: mp,
		MaxConcurrency: 2, // Concurrency limit 2 for this test
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Launch 2 requests that block in provider
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := bytes.NewReader([]byte(`{"path":"/"}`))
			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/course/list", body)
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
	}

	// Wait until both enter
	for {
		mu.Lock()
		c := enteredCount
		mu.Unlock()
		if c == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 3rd request should be rejected immediately with 429
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/course/list", bytes.NewReader([]byte(`{"path":"/"}`)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("failed 3rd request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 Too Many Requests, got %d", resp.StatusCode)
	}

	// Unblock active requests and finish
	close(blocked)
	wg.Wait()
}

// blockingReader blocks on Read until closed
type blockingReader struct {
	closed chan struct{}
}

func (b *blockingReader) Read(p []byte) (n int, err error) {
	<-b.closed
	return 0, io.EOF
}

func (b *blockingReader) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

func TestServer_StalledBodyReleasesConcurrency(t *testing.T) {
	token := "secret-test-token"
	tokenHex := hashToken(token)
	// Grant expires in 100ms to enforce quick timeout
	expiry := time.Now().Add(100 * time.Millisecond).UTC().Format(time.RFC3339)
	grantsJSON := fmt.Sprintf(`{"grants":[{"token_sha256":"%s","run_id":"0123456789abcdef0123456789abcdef","expires_at":"%s","scopes":["course:read"]}]}`, tokenHex, expiry)
	grantsPath := createTestGrantsFile(t, grantsJSON, 0600)

	srv, err := NewServer(ServerConfig{
		Addr:           "127.0.0.1:0",
		Authorizer:     NewFileAuthorizer(grantsPath),
		CourseProvider: &mockProvider{},
		MaxConcurrency: 1, // Concurrency limit 1
		DefaultTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Use raw TCP connection to send partial request body and stall
	u, err := ts.Config.Addr, nil
	_ = u
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer conn.Close()

	// Send headers with Content-Length 100 but send no body bytes
	header := fmt.Sprintf("POST /v1/course/list HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n", ts.Listener.Addr().String(), token)
	if _, err := conn.Write([]byte(header)); err != nil {
		t.Fatalf("failed to write headers: %v", err)
	}

	// Server should abort reading after deadline (100ms) and close or return error
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 1024)
	for {
		_, rErr := conn.Read(buf)
		if rErr != nil {
			// Read error or EOF indicates socket read was interrupted
			break
		}
	}

	// Verify semaphore concurrency slot was released
	// Update grant expiry so second request succeeds
	newExpiry := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	newGrantsJSON := fmt.Sprintf(`{"grants":[{"token_sha256":"%s","run_id":"0123456789abcdef0123456789abcdef","expires_at":"%s","scopes":["course:read"]}]}`, tokenHex, newExpiry)
	if err := os.WriteFile(grantsPath, []byte(newGrantsJSON), 0600); err != nil {
		t.Fatal(err)
	}

	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/course/list", bytes.NewReader([]byte(`{"path":"/"}`)))
	req2.Header.Set("Authorization", "Bearer "+token)
	req2.Header.Set("Content-Type", "application/json")

	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("subsequent request failed: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode == http.StatusTooManyRequests {
		t.Fatal("semaphore was not released after stalled connection timed out")
	}
}
