package toolserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
	"unicode/utf8"
)

// FileAuthorizer reloads grants from TJUCLI_GRANTS_FILE on each authorization request.
type FileAuthorizer struct {
	filePath string
	nowFunc  func() time.Time
}

// NewFileAuthorizer creates a FileAuthorizer backed by the given file path.
func NewFileAuthorizer(filePath string) *FileAuthorizer {
	return &FileAuthorizer{
		filePath: filePath,
		nowFunc:  time.Now,
	}
}

// Authorize checks if the bearer token is authorized for requiredScope.
func (a *FileAuthorizer) Authorize(_ context.Context, token, requiredScope string) (*Grant, *AuthError) {
	if token == "" {
		return nil, &AuthError{
			StatusCode: http.StatusUnauthorized,
			Code:       "unauthorized",
			Message:    "missing or empty bearer token",
		}
	}

	tokenHash := sha256.Sum256([]byte(token))
	tokenHex := hex.EncodeToString(tokenHash[:])

	grants, err := readAndValidateGrantsDoc(a.filePath)
	if err != nil {
		return nil, &AuthError{
			StatusCode: http.StatusServiceUnavailable,
			Code:       "service_unavailable",
			Message:    "authorization service unavailable",
		}
	}

	now := time.Now()
	if a.nowFunc != nil {
		now = a.nowFunc()
	}

	var matched *Grant
	for i := range grants {
		g := &grants[i]
		if subtle.ConstantTimeCompare([]byte(g.TokenSHA256), []byte(tokenHex)) == 1 {
			matched = g
		}
	}

	if matched == nil {
		return nil, &AuthError{
			StatusCode: http.StatusUnauthorized,
			Code:       "unauthorized",
			Message:    "invalid or expired token",
		}
	}

	// Verify expiry against current time
	expiresAt, err := time.Parse(time.RFC3339, matched.ExpiresAt)
	if err != nil || !expiresAt.After(now) {
		return nil, &AuthError{
			StatusCode: http.StatusUnauthorized,
			Code:       "unauthorized",
			Message:    "invalid or expired token",
		}
	}

	hasScope := false
	for _, s := range matched.Scopes {
		if s == requiredScope {
			hasScope = true
			break
		}
	}

	if !hasScope {
		return nil, &AuthError{
			StatusCode: http.StatusForbidden,
			Code:       "forbidden",
			Message:    "insufficient grant scope",
		}
	}

	return matched, nil
}

// ValidateGrantsFile checks that the grants file at path exists, has 0600 permissions,
// is <=64KiB, contains valid JSON without unknown fields, and has valid grant entries.
func ValidateGrantsFile(path string) error {
	_, err := readAndValidateGrantsDoc(path)
	return err
}

// readAndValidateGrantsDoc securely opens and parses the grants file.
// It uses Lstat + Open + Fstat (SameFile) to verify permissions (0600) and regular file status
// without race condition vulnerabilities, bounded io.ReadAll, and rejects the entire document
// if there are syntax errors, unknown fields, trailing content, duplicate tokens, duplicate run_ids,
// or invalid formats. Errors are generic to prevent leaking sensitive file contents.
func readAndValidateGrantsDoc(path string) ([]Grant, error) {
	if path == "" {
		return nil, errors.New("grants file path is empty")
	}

	lstatInfo, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("cannot stat grants file")
	}
	if !lstatInfo.Mode().IsRegular() {
		return nil, errors.New("grants file must be a regular file")
	}
	if lstatInfo.Mode().Perm() != 0600 {
		return nil, fmt.Errorf("grants file permissions are %04o, want 0600", lstatInfo.Mode().Perm())
	}
	if lstatInfo.Size() > MaxGrantsFileBytes {
		return nil, errors.New("grants file exceeds maximum allowed size")
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open grants file")
	}
	defer file.Close()

	fstatInfo, err := file.Stat()
	if err != nil {
		return nil, errors.New("cannot fstat grants file")
	}
	if !os.SameFile(lstatInfo, fstatInfo) {
		return nil, errors.New("grants file changed during opening")
	}
	if !fstatInfo.Mode().IsRegular() {
		return nil, errors.New("opened grants file is not a regular file")
	}
	if fstatInfo.Mode().Perm() != 0600 {
		return nil, fmt.Errorf("opened grants file permissions are %04o, want 0600", fstatInfo.Mode().Perm())
	}

	data, err := io.ReadAll(io.LimitReader(file, MaxGrantsFileBytes+1))
	if err != nil {
		return nil, errors.New("cannot read grants file")
	}
	if int64(len(data)) > MaxGrantsFileBytes {
		return nil, errors.New("grants file exceeds maximum allowed size")
	}
	if !utf8.Valid(data) {
		return nil, errors.New("grants file is not valid UTF-8")
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var doc GrantsFile
	if err := dec.Decode(&doc); err != nil {
		return nil, errors.New("malformed grants file JSON")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("trailing content in grants file JSON")
	}

	seenTokens := make(map[string]struct{}, len(doc.Grants))
	seenRunIDs := make(map[string]struct{}, len(doc.Grants))

	for index, g := range doc.Grants {
		if !isLowerHex(g.TokenSHA256, 64) {
			return nil, fmt.Errorf("grant[%d]: invalid token_sha256 format", index)
		}
		if !isLowerHex(g.RunID, 32) {
			return nil, fmt.Errorf("grant[%d]: invalid run_id format", index)
		}
		if _, err := time.Parse(time.RFC3339, g.ExpiresAt); err != nil {
			return nil, fmt.Errorf("grant[%d]: invalid expires_at format", index)
		}
		if len(g.Scopes) == 0 {
			return nil, fmt.Errorf("grant[%d]: scopes must not be empty", index)
		}
		for sIdx, s := range g.Scopes {
			if s == "" {
				return nil, fmt.Errorf("grant[%d]: scope[%d] must not be empty", index, sIdx)
			}
		}
		if _, dup := seenTokens[g.TokenSHA256]; dup {
			return nil, fmt.Errorf("grant[%d]: duplicate token_sha256", index)
		}
		if _, dup := seenRunIDs[g.RunID]; dup {
			return nil, fmt.Errorf("grant[%d]: duplicate run_id", index)
		}
		seenTokens[g.TokenSHA256] = struct{}{}
		seenRunIDs[g.RunID] = struct{}{}
	}

	return doc.Grants, nil
}

func isLowerHex(s string, expectedLen int) bool {
	if len(s) != expectedLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		if (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') {
			continue
		}
		return false
	}
	return true
}
