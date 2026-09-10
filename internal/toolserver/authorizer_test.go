package toolserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func createTestGrantsFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "grants.json")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatalf("failed to write grants file: %v", err)
	}
	return p
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func TestFileAuthorizer_Authorize_Success(t *testing.T) {
	token := "valid-secret-token-12345"
	tokenHex := hashToken(token)
	expiry := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	runID := "0123456789abcdef0123456789abcdef"

	jsonContent := fmt.Sprintf(`{
		"grants": [
			{
				"token_sha256": "%s",
				"run_id": "%s",
				"expires_at": "%s",
				"scopes": ["course:read"]
			}
		]
	}`, tokenHex, runID, expiry)

	path := createTestGrantsFile(t, jsonContent, 0600)
	auth := NewFileAuthorizer(path)

	grant, err := auth.Authorize(context.Background(), token, "course:read")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if grant.RunID != runID {
		t.Fatalf("expected run_id %s, got %s", runID, grant.RunID)
	}
}

func TestFileAuthorizer_Authorize_MissingOrInvalidToken(t *testing.T) {
	tokenHex := hashToken("token")
	expiry := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	jsonContent := fmt.Sprintf(`{"grants":[{"token_sha256":"%s","run_id":"0123456789abcdef0123456789abcdef","expires_at":"%s","scopes":["course:read"]}]}`, tokenHex, expiry)
	path := createTestGrantsFile(t, jsonContent, 0600)
	auth := NewFileAuthorizer(path)

	// Missing token
	_, authErr := auth.Authorize(context.Background(), "", "course:read")
	if authErr == nil || authErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 AuthError, got %v", authErr)
	}

	// Wrong token
	_, authErr = auth.Authorize(context.Background(), "wrong-token", "course:read")
	if authErr == nil || authErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 AuthError, got %v", authErr)
	}
}

func TestFileAuthorizer_Authorize_Expired(t *testing.T) {
	token := "expired-token"
	tokenHex := hashToken(token)
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	jsonContent := fmt.Sprintf(`{"grants":[{"token_sha256":"%s","run_id":"0123456789abcdef0123456789abcdef","expires_at":"%s","scopes":["course:read"]}]}`, tokenHex, past)
	path := createTestGrantsFile(t, jsonContent, 0600)
	auth := NewFileAuthorizer(path)

	_, authErr := auth.Authorize(context.Background(), token, "course:read")
	if authErr == nil || authErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 AuthError, got %v", authErr)
	}
}

func TestFileAuthorizer_Authorize_MissingScope(t *testing.T) {
	token := "scope-test-token"
	tokenHex := hashToken(token)
	expiry := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	jsonContent := fmt.Sprintf(`{"grants":[{"token_sha256":"%s","run_id":"0123456789abcdef0123456789abcdef","expires_at":"%s","scopes":["other:scope"]}]}`, tokenHex, expiry)
	path := createTestGrantsFile(t, jsonContent, 0600)
	auth := NewFileAuthorizer(path)

	_, authErr := auth.Authorize(context.Background(), token, "course:read")
	if authErr == nil || authErr.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 AuthError, got %v", authErr)
	}
}

func TestFileAuthorizer_Authorize_PermissionsFailClosed(t *testing.T) {
	token := "perm-test-token"
	tokenHex := hashToken(token)
	expiry := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	jsonContent := fmt.Sprintf(`{"grants":[{"token_sha256":"%s","run_id":"0123456789abcdef0123456789abcdef","expires_at":"%s","scopes":["course:read"]}]}`, tokenHex, expiry)

	// Mode 0644 should fail closed
	path := createTestGrantsFile(t, jsonContent, 0644)
	auth := NewFileAuthorizer(path)

	_, authErr := auth.Authorize(context.Background(), token, "course:read")
	if authErr == nil || authErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 AuthError, got %v", authErr)
	}
}

func TestFileAuthorizer_Authorize_ReloadRevocation(t *testing.T) {
	token := "revokable-token"
	tokenHex := hashToken(token)
	expiry := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	jsonContent := fmt.Sprintf(`{"grants":[{"token_sha256":"%s","run_id":"0123456789abcdef0123456789abcdef","expires_at":"%s","scopes":["course:read"]}]}`, tokenHex, expiry)

	path := createTestGrantsFile(t, jsonContent, 0600)
	auth := NewFileAuthorizer(path)

	// First request succeeds
	_, err := auth.Authorize(context.Background(), token, "course:read")
	if err != nil {
		t.Fatalf("unexpected error before revocation: %v", err)
	}

	// Revoke by overwriting grants file with empty list
	revokedContent := `{"grants":[]}`
	if err := os.WriteFile(path, []byte(revokedContent), 0600); err != nil {
		t.Fatalf("failed to overwrite grants file: %v", err)
	}

	// Next request reloads and fails with 401
	_, authErr := auth.Authorize(context.Background(), token, "course:read")
	if authErr == nil || authErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 AuthError, got %v", authErr)
	}
}

func TestFileAuthorizer_Authorize_ReloadRejectsDuplicate(t *testing.T) {
	token := "dup-token"
	tokenHex := hashToken(token)
	expiry := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	jsonContent := fmt.Sprintf(`{"grants":[{"token_sha256":"%s","run_id":"0123456789abcdef0123456789abcdef","expires_at":"%s","scopes":["course:read"]}]}`, tokenHex, expiry)

	path := createTestGrantsFile(t, jsonContent, 0600)
	auth := NewFileAuthorizer(path)

	// Succeeded initially
	_, err := auth.Authorize(context.Background(), token, "course:read")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Update grants with a duplicate token entry
	dupContent := fmt.Sprintf(`{"grants":[
		{"token_sha256":"%s","run_id":"0123456789abcdef0123456789abcdef","expires_at":"%s","scopes":["course:read"]},
		{"token_sha256":"%s","run_id":"fedcba9876543210fedcba9876543210","expires_at":"%s","scopes":["course:read"]}
	]}`, tokenHex, expiry, tokenHex, expiry)
	if err := os.WriteFile(path, []byte(dupContent), 0600); err != nil {
		t.Fatalf("failed to overwrite with duplicates: %v", err)
	}

	// Reload must reject the entire file with 503 rather than authorizing the first duplicate
	_, authErr := auth.Authorize(context.Background(), token, "course:read")
	if authErr == nil || authErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 service unavailable on duplicate reload, got %v", authErr)
	}
}

func TestValidateGrantsFile(t *testing.T) {
	validTokenHex := hashToken("valid")
	validRunID := "0123456789abcdef0123456789abcdef"
	validExpiry := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)

	t.Run("valid configuration", func(t *testing.T) {
		content := fmt.Sprintf(`{"grants":[{"token_sha256":"%s","run_id":"%s","expires_at":"%s","scopes":["course:read"]}]}`, validTokenHex, validRunID, validExpiry)
		p := createTestGrantsFile(t, content, 0600)
		if err := ValidateGrantsFile(p); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("wrong permissions", func(t *testing.T) {
		content := fmt.Sprintf(`{"grants":[{"token_sha256":"%s","run_id":"%s","expires_at":"%s","scopes":["course:read"]}]}`, validTokenHex, validRunID, validExpiry)
		p := createTestGrantsFile(t, content, 0666)
		if err := ValidateGrantsFile(p); err == nil {
			t.Fatal("expected error for 0666 permissions")
		}
	})

	t.Run("unknown fields", func(t *testing.T) {
		content := fmt.Sprintf(`{"grants":[{"token_sha256":"%s","run_id":"%s","expires_at":"%s","scopes":["course:read"],"extra":"bad"}]}`, validTokenHex, validRunID, validExpiry)
		p := createTestGrantsFile(t, content, 0600)
		if err := ValidateGrantsFile(p); err == nil {
			t.Fatal("expected error for unknown fields")
		}
	})

	t.Run("duplicate tokens", func(t *testing.T) {
		content := fmt.Sprintf(`{"grants":[
			{"token_sha256":"%s","run_id":"%s","expires_at":"%s","scopes":["course:read"]},
			{"token_sha256":"%s","run_id":"abcdef0123456789abcdef0123456789","expires_at":"%s","scopes":["course:read"]}
		]}`, validTokenHex, validRunID, validExpiry, validTokenHex, validExpiry)
		p := createTestGrantsFile(t, content, 0600)
		if err := ValidateGrantsFile(p); err == nil {
			t.Fatal("expected error for duplicate token_sha256")
		}
	})

	t.Run("duplicate run_id", func(t *testing.T) {
		tokenHex2 := hashToken("another")
		content := fmt.Sprintf(`{"grants":[
			{"token_sha256":"%s","run_id":"%s","expires_at":"%s","scopes":["course:read"]},
			{"token_sha256":"%s","run_id":"%s","expires_at":"%s","scopes":["course:read"]}
		]}`, validTokenHex, validRunID, validExpiry, tokenHex2, validRunID, validExpiry)
		p := createTestGrantsFile(t, content, 0600)
		if err := ValidateGrantsFile(p); err == nil {
			t.Fatal("expected error for duplicate run_id")
		}
	})
}
