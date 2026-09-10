package toolserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yunzaixi-dev/tjucli/internal/tjucli"
)

// ServerConfig holds the configuration for the ToolServer.
type ServerConfig struct {
	Addr           string
	GrantsFilePath string
	MaxConcurrency int
	DefaultTimeout time.Duration
	SpoolBaseDir   string
	Authorizer     Authorizer
	CourseProvider CourseProvider
}

// Server encapsulates the HTTP server, routing, authorization, and concurrency controls.
type Server struct {
	cfg        ServerConfig
	authorizer Authorizer
	provider   CourseProvider
	sem        chan struct{}
	httpServer *http.Server
	spoolDir   string
	spoolMu    sync.Mutex
}

// NewServer initializes a new Server with the provided configuration.
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Addr == "" {
		cfg.Addr = DefaultAddr
	}
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = DefaultConcurrency
	}
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = DefaultRequestTimeout
	}

	authorizer := cfg.Authorizer
	if authorizer == nil {
		if cfg.GrantsFilePath == "" {
			return nil, errors.New("grants file path is required when Authorizer is nil")
		}
		if err := ValidateGrantsFile(cfg.GrantsFilePath); err != nil {
			return nil, fmt.Errorf("invalid grants file: %w", err)
		}
		authorizer = NewFileAuthorizer(cfg.GrantsFilePath)
	}

	provider := cfg.CourseProvider
	if provider == nil {
		var err error
		provider, err = tjucli.NewProvider()
		if err != nil {
			return nil, fmt.Errorf("failed to initialize course provider: %w", err)
		}
	}

	// Always create a dedicated child directory inside SpoolBaseDir (or system temp if empty)
	// Never delete SpoolBaseDir itself.
	spoolDir, err := os.MkdirTemp(cfg.SpoolBaseDir, "tjucli-spool-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temporary spool directory: %w", err)
	}
	// Enforce 0700 permissions on the created spool directory
	_ = os.Chmod(spoolDir, 0700)

	s := &Server{
		cfg:        cfg,
		authorizer: authorizer,
		provider:   provider,
		sem:        make(chan struct{}, cfg.MaxConcurrency),
		spoolDir:   spoolDir,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /v1/course/list", s.withAuthAndLimit(RequiredScopeCourseRead, s.handleCourseList))
	mux.HandleFunc("POST /v1/course/search", s.withAuthAndLimit(RequiredScopeCourseRead, s.handleCourseSearch))
	mux.HandleFunc("POST /v1/course/download", s.withAuthAndLimit(RequiredScopeCourseRead, s.handleCourseDownload))

	s.httpServer = &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.DefaultTimeout + 15*time.Second,
		WriteTimeout:      cfg.DefaultTimeout + 15*time.Second,
		IdleTimeout:       120 * time.Second,
	}

	return s, nil
}

// Handler returns the http.Handler for testing with httptest.
func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe() error {
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully shuts down the server, closing active connections, then cleans up the spool dir.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.httpServer.Shutdown(ctx)
	if err != nil {
		_ = s.httpServer.Close()
	}
	s.cleanupSpoolDir()
	return err
}

// Close immediately closes the server and cleans up the spool dir.
func (s *Server) Close() error {
	err := s.httpServer.Close()
	s.cleanupSpoolDir()
	return err
}

func (s *Server) cleanupSpoolDir() {
	s.spoolMu.Lock()
	defer s.spoolMu.Unlock()
	if s.spoolDir != "" {
		_ = os.RemoveAll(s.spoolDir)
		s.spoolDir = ""
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeFailure(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}` + "\n"))
}

// withAuthAndLimit wraps a handler with:
// 1. HTTP method check (POST only)
// 2. Concurrency check (reject with 429 if semaphore is full)
// 3. Bearer token extraction and authorization against requiredScope
// 4. Per-request ResponseController read/write deadlines clamped to min(config timeout, expiry)
// 5. Explicit check for grant expiration before invoking handler
func (s *Server) withAuthAndLimit(requiredScope string, next func(w http.ResponseWriter, r *http.Request, grant *Grant)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeFailure(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}

		// Concurrency limiting (max 4 concurrent requests across /v1)
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		default:
			writeFailure(w, http.StatusTooManyRequests, "busy", "server concurrency limit reached")
			return
		}

		// Bearer token extraction
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			writeFailure(w, http.StatusUnauthorized, "unauthorized", "missing Authorization header")
			return
		}
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
			writeFailure(w, http.StatusUnauthorized, "unauthorized", "invalid Authorization header format, expected Bearer <token>")
			return
		}
		token := strings.TrimSpace(parts[1])

		grant, authErr := s.authorizer.Authorize(r.Context(), token, requiredScope)
		if authErr != nil {
			writeFailure(w, authErr.StatusCode, authErr.Code, authErr.Message)
			return
		}

		// Check expiry timestamp
		exp, err := time.Parse(time.RFC3339, grant.ExpiresAt)
		if err != nil {
			writeFailure(w, http.StatusUnauthorized, "unauthorized", "invalid grant expiration")
			return
		}
		now := time.Now()
		remaining := exp.Sub(now)
		if remaining <= 0 {
			writeFailure(w, http.StatusUnauthorized, "unauthorized", "grant has expired")
			return
		}

		timeout := s.cfg.DefaultTimeout
		if remaining < timeout {
			timeout = remaining
		}

		// Set socket read/write deadlines via ResponseController
		deadline := time.Now().Add(timeout)
		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(deadline)
		_ = rc.SetWriteDeadline(deadline)

		ctx, cancel := context.WithDeadline(r.Context(), deadline)
		defer cancel()

		r = r.WithContext(ctx)
		next(w, r, grant)
	}
}

func (s *Server) handleCourseList(w http.ResponseWriter, r *http.Request, _ *Grant) {
	var req CourseListRequest
	if err := decodeStrictJSON(w, r, &req); err != nil {
		return
	}

	result, meta, cliErr := s.provider.List(r.Context(), req.Path, req.Cursor)
	if cliErr != nil {
		writeProviderError(w, cliErr)
		return
	}

	if err := r.Context().Err(); err != nil {
		return
	}

	writeSuccess(w, http.StatusOK, result, meta)
}

func (s *Server) handleCourseSearch(w http.ResponseWriter, r *http.Request, _ *Grant) {
	var req CourseSearchRequest
	if err := decodeStrictJSON(w, r, &req); err != nil {
		return
	}

	if strings.TrimSpace(req.Query) == "" {
		writeFailure(w, http.StatusBadRequest, "invalid_argument", "query must not be empty")
		return
	}

	maxPages := 20
	if req.MaxPages != nil {
		maxPages = *req.MaxPages
	}
	limit := 50
	if req.Limit != nil {
		limit = *req.Limit
	}

	if maxPages <= 0 || limit <= 0 {
		writeFailure(w, http.StatusBadRequest, "invalid_argument", "max_pages and limit must be positive integers")
		return
	}

	result, meta, cliErr := s.provider.Search(r.Context(), req.Query, maxPages, limit)
	if cliErr != nil {
		writeProviderError(w, cliErr)
		return
	}

	if err := r.Context().Err(); err != nil {
		return
	}

	writeSuccess(w, http.StatusOK, result, meta)
}

func (s *Server) handleCourseDownload(w http.ResponseWriter, r *http.Request, _ *Grant) {
	var req CourseDownloadRequest
	if err := decodeStrictJSON(w, r, &req); err != nil {
		return
	}

	if strings.TrimSpace(req.Path) == "" {
		writeFailure(w, http.StatusBadRequest, "invalid_argument", "path must not be empty")
		return
	}

	maxBytes := MaxDownloadBytes
	if req.MaxBytes != nil {
		if *req.MaxBytes <= 0 {
			writeFailure(w, http.StatusBadRequest, "invalid_argument", "max_bytes must be greater than 0")
			return
		}
		if *req.MaxBytes > MaxDownloadBytes {
			writeFailure(w, http.StatusBadRequest, "invalid_argument", fmt.Sprintf("max_bytes exceeds limit of %d bytes", MaxDownloadBytes))
			return
		}
		maxBytes = *req.MaxBytes
	}

	s.spoolMu.Lock()
	baseSpool := s.spoolDir
	s.spoolMu.Unlock()
	if baseSpool == "" {
		writeFailure(w, http.StatusServiceUnavailable, "service_unavailable", "spool directory unavailable")
		return
	}

	// Create request-isolated temporary spool directory with 0700
	reqSpoolDir, err := os.MkdirTemp(baseSpool, "dl-*")
	if err != nil {
		writeFailure(w, http.StatusServiceUnavailable, "service_unavailable", "failed to initialize download spool")
		return
	}
	_ = os.Chmod(reqSpoolDir, 0700)
	defer os.RemoveAll(reqSpoolDir)

	targetFilePath := filepath.Join(reqSpoolDir, "payload.bin")

	// Call Provider.Download
	_, cliErr := s.provider.Download(r.Context(), req.Path, targetFilePath, maxBytes)
	if cliErr != nil {
		writeProviderError(w, cliErr)
		return
	}

	if err := r.Context().Err(); err != nil {
		return
	}

	// Verify the spooled file
	file, err := os.Open(targetFilePath)
	if err != nil {
		writeFailure(w, http.StatusBadGateway, "upstream_error", "downloaded payload is inaccessible")
		return
	}
	defer file.Close()

	fi, err := file.Stat()
	if err != nil {
		writeFailure(w, http.StatusBadGateway, "upstream_error", "downloaded payload cannot be verified")
		return
	}

	actualSize := fi.Size()
	if actualSize > maxBytes {
		writeFailure(w, http.StatusBadRequest, "file_too_large", "downloaded file exceeds max_bytes")
		return
	}

	// Hash calculation
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		writeFailure(w, http.StatusBadGateway, "upstream_error", "failed to read spooled download payload")
		return
	}
	shaHex := hex.EncodeToString(h.Sum(nil))

	// Rewind file for streaming
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeFailure(w, http.StatusBadGateway, "upstream_error", "failed to stream spooled download payload")
		return
	}

	if err := r.Context().Err(); err != nil {
		return
	}

	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", actualSize))
	w.Header().Set("X-Content-SHA256", shaHex)
	w.WriteHeader(http.StatusOK)

	// Stream with cancellation awareness
	buf := make([]byte, 32*1024)
	for {
		if err := r.Context().Err(); err != nil {
			return
		}
		n, rErr := file.Read(buf)
		if n > 0 {
			if _, wErr := w.Write(buf[:n]); wErr != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if rErr != nil {
			break
		}
	}
}

func decodeStrictJSON(w http.ResponseWriter, r *http.Request, dest any) error {
	rawContentType := r.Header.Get("Content-Type")
	if rawContentType == "" {
		writeFailure(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return errors.New("missing content-type")
	}

	mediaType, _, err := mime.ParseMediaType(rawContentType)
	if err != nil || mediaType != "application/json" {
		writeFailure(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return errors.New("unsupported media type")
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBodyBytes+1))
	if err != nil {
		if r.Context().Err() != nil {
			return r.Context().Err()
		}
		writeFailure(w, http.StatusBadRequest, "invalid_argument", "failed to read request body")
		return err
	}
	if int64(len(bodyBytes)) > MaxRequestBodyBytes {
		writeFailure(w, http.StatusRequestEntityTooLarge, "payload_too_large", fmt.Sprintf("request body exceeds %d bytes", MaxRequestBodyBytes))
		return errors.New("payload too large")
	}

	if !utf8.Valid(bodyBytes) {
		writeFailure(w, http.StatusBadRequest, "invalid_argument", "request body is not valid UTF-8")
		return errors.New("invalid utf8")
	}

	dec := json.NewDecoder(bytes.NewReader(bodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		writeFailure(w, http.StatusBadRequest, "invalid_argument", "malformed JSON request")
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		writeFailure(w, http.StatusBadRequest, "invalid_argument", "trailing content in request JSON")
		return errors.New("trailing content")
	}

	return nil
}

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func writeSuccess(w http.ResponseWriter, statusCode int, data any, meta any) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)

	env := tjucli.SuccessEnvelope{
		OK:   true,
		Data: data,
		Meta: meta,
	}
	_ = json.NewEncoder(w).Encode(env)
}

func writeFailure(w http.ResponseWriter, statusCode int, code, message string) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)

	env := tjucli.FailureEnvelope{
		OK: false,
		Error: tjucli.CLIErrorPayload{
			Code:    code,
			Message: message,
		},
	}
	_ = json.NewEncoder(w).Encode(env)
}

func writeProviderError(w http.ResponseWriter, cliErr *tjucli.CLIError) {
	if cliErr == nil {
		writeFailure(w, http.StatusInternalServerError, "internal_error", "an unexpected error occurred")
		return
	}

	statusCode := http.StatusBadGateway
	code := cliErr.Code
	msg := cliErr.Message

	switch cliErr.Code {
	case "not_found":
		statusCode = http.StatusNotFound
	case "invalid_argument":
		statusCode = http.StatusBadRequest
	case "file_too_large":
		statusCode = http.StatusBadRequest
	default:
		// Map upstream network/provider errors to 502 Bad Gateway and redact internal URLs/tokens
		statusCode = http.StatusBadGateway
		code = "upstream_error"
		msg = "upstream provider error occurred"
	}

	writeFailure(w, statusCode, code, msg)
}
