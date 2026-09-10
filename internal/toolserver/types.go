package toolserver

import (
	"context"
	"time"

	"github.com/yunzaixi-dev/tjucli/internal/tjucli"
)

const (
	DefaultAddr             = "127.0.0.1:18090"
	MaxRequestBodyBytes     = 16 * 1024       // 16KiB
	MaxGrantsFileBytes      = 64 * 1024       // 64KiB
	MaxDownloadBytes        = int64(64 << 20) // 64MiB
	DefaultConcurrency      = 4
	DefaultRequestTimeout   = 120 * time.Second
	RequiredScopeCourseRead = "course:read"
)

// Grant models one authorized scoped run token entry.
type Grant struct {
	TokenSHA256 string   `json:"token_sha256"`
	RunID       string   `json:"run_id"`
	ExpiresAt   string   `json:"expires_at"`
	Scopes      []string `json:"scopes"`
}

// GrantsFile models the grants array in TJUCLI_GRANTS_FILE.
type GrantsFile struct {
	Grants []Grant `json:"grants"`
}

// CourseListRequest models POST /v1/course/list JSON body.
type CourseListRequest struct {
	Path   string `json:"path"`
	Cursor string `json:"cursor"`
}

// CourseSearchRequest models POST /v1/course/search JSON body.
type CourseSearchRequest struct {
	Query    string `json:"query"`
	MaxPages *int   `json:"max_pages"`
	Limit    *int   `json:"limit"`
}

// CourseDownloadRequest models POST /v1/course/download JSON body.
// Note: no caller output/filesystem path is accepted.
type CourseDownloadRequest struct {
	Path     string `json:"path"`
	MaxBytes *int64 `json:"max_bytes"`
}

// CourseProvider defines the course catalog and download operations.
// Satisfied by *tjucli.Provider and test mocks.
type CourseProvider interface {
	List(ctx context.Context, providerPath, cursor string) (tjucli.ListResult, tjucli.ListMeta, *tjucli.CLIError)
	Search(ctx context.Context, query string, maxPages, limit int) (tjucli.SearchResult, tjucli.SearchMeta, *tjucli.CLIError)
	Download(ctx context.Context, providerPath, outputPath string, maxBytes int64) (tjucli.DownloadResult, *tjucli.CLIError)
}

// AuthError represents an authentication or authorization failure.
type AuthError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *AuthError) Error() string {
	return e.Message
}

// Authorizer authorizes incoming bearer tokens against required scopes.
type Authorizer interface {
	Authorize(ctx context.Context, token, requiredScope string) (*Grant, *AuthError)
}
