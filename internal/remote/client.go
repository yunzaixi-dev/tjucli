package remote

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yunzaixi-dev/tjucli/internal/tjucli"
)

const (
	MaxJSONRequestBytes  = 16 * 1024       // 16 KiB limit for request bodies
	MaxJSONResponseBytes = 4 * 1024 * 1024 // 4 MiB limit for response bodies
	MaxTokenFileBytes    = 4 * 1024        // 4 KiB
	MinTokenLengthBytes  = 32              // minimum token length in bytes
	MaxCursorBytes       = 8192            // consistent with direct provider
	MaxDownloadBytes     = int64(64 << 20) // 64 MiB limit for downloads
	DefaultHTTPTimeout   = 120 * time.Second
)

// Config encapsulates validated remote connection parameters.
type Config struct {
	BaseURL *url.URL
	Token   string
}

// Client implements courseProvider over HTTP/JSON.
type Client struct {
	cfg        Config
	httpClient *http.Client
}

// LoadConfig reads and strictly validates the remote environment variables.
// Redacts secrets, URLs, and file paths from errors.
func LoadConfig() (Config, *tjucli.CLIError) {
	serverURLStr := strings.TrimSpace(os.Getenv("TJUCLI_SERVER_URL"))
	if serverURLStr == "" {
		return Config{}, tjucli.NewRuntimeError("configuration_error", "TJUCLI_SERVER_URL is required in remote mode")
	}

	tokenFilePath := strings.TrimSpace(os.Getenv("TJUCLI_TOKEN_FILE"))
	if tokenFilePath == "" {
		return Config{}, tjucli.NewRuntimeError("configuration_error", "TJUCLI_TOKEN_FILE is required in remote mode")
	}

	parsedURL, err := validateServerURL(serverURLStr)
	if err != nil {
		return Config{}, tjucli.NewRuntimeError("configuration_error", err.Error())
	}

	token, err := loadTokenFromFile(tokenFilePath)
	if err != nil {
		return Config{}, tjucli.NewRuntimeError("configuration_error", err.Error())
	}

	return Config{
		BaseURL: parsedURL,
		Token:   token,
	}, nil
}

func validateServerURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("invalid TJUCLI_SERVER_URL")
	}
	if u.User != nil {
		return nil, errors.New("TJUCLI_SERVER_URL must not contain user credentials")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return nil, errors.New("TJUCLI_SERVER_URL must not contain query parameters")
	}
	if u.Fragment != "" {
		return nil, errors.New("TJUCLI_SERVER_URL must not contain fragments")
	}
	if u.Opaque != "" {
		return nil, errors.New("TJUCLI_SERVER_URL must not be opaque")
	}
	if u.RawPath != "" {
		return nil, errors.New("TJUCLI_SERVER_URL must not contain raw path formatting")
	}

	hostname := strings.ToLower(u.Hostname())
	if hostname == "" {
		return nil, errors.New("TJUCLI_SERVER_URL host cannot be empty")
	}

	isLoopback := false
	if hostname == "localhost" {
		isLoopback = true
	} else if ip := net.ParseIP(hostname); ip != nil {
		if ip.IsLoopback() {
			isLoopback = true
		}
	}

	if u.Scheme == "https" {
		// Valid https
	} else if u.Scheme == "http" {
		if !isLoopback {
			return nil, errors.New("TJUCLI_SERVER_URL must use HTTPS for non-loopback hosts")
		}
	} else {
		return nil, errors.New("TJUCLI_SERVER_URL must use https or http")
	}

	cleanedPath := strings.TrimRight(u.Path, "/")
	if cleanedPath != "" {
		return nil, errors.New("TJUCLI_SERVER_URL must not contain a path prefix")
	}

	clone := *u
	clone.Path = ""
	return &clone, nil
}

func loadTokenFromFile(filePath string) (string, error) {
	fi, err := os.Lstat(filePath)
	if err != nil {
		return "", errors.New("cannot read token file")
	}
	if !fi.Mode().IsRegular() {
		return "", errors.New("token file must be a regular file")
	}

	// Permission check on Unix: owner-only (perm & 0077 == 0)
	if !isWindows() {
		if fi.Mode().Perm()&0077 != 0 {
			return "", errors.New("token file permissions must restrict group and other access")
		}
	}

	if fi.Size() > MaxTokenFileBytes {
		return "", errors.New("token file is too large")
	}

	f, err := os.Open(filePath)
	if err != nil {
		return "", errors.New("cannot open token file")
	}
	defer f.Close()

	// Verify opened file descriptor refers to the same regular file (prevent TOCTOU symlink race)
	openedFI, err := f.Stat()
	if err != nil {
		return "", errors.New("cannot inspect opened token file")
	}
	if !openedFI.Mode().IsRegular() {
		return "", errors.New("token file must be a regular file")
	}
	if !os.SameFile(fi, openedFI) {
		return "", errors.New("token file changed during opening")
	}
	if !isWindows() {
		if openedFI.Mode().Perm()&0077 != 0 {
			return "", errors.New("token file permissions must restrict group and other access")
		}
	}

	data, err := io.ReadAll(io.LimitReader(f, MaxTokenFileBytes+1))
	if err != nil {
		return "", errors.New("failed to read token file")
	}
	if int64(len(data)) > MaxTokenFileBytes {
		return "", errors.New("token file is too large")
	}
	if !utf8.Valid(data) {
		return "", errors.New("token file is not valid UTF-8")
	}

	token := strings.TrimSpace(string(data))
	if len(token) < MinTokenLengthBytes {
		return "", errors.New("token must have at least 32 characters in length (generate with a cryptographically secure random generator)")
	}

	for _, r := range token {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return "", errors.New("token must not contain control characters or spaces")
		}
	}

	return token, nil
}

// NewClient creates a new remote provider client.
func NewClient(cfg Config) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   DefaultHTTPTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// List implements courseProvider.List.
func (c *Client) List(ctx context.Context, providerPath, cursor string) (tjucli.ListResult, tjucli.ListMeta, *tjucli.CLIError) {
	normPath, normErr := normalizeProviderPath(providerPath)
	if normErr != nil {
		return tjucli.ListResult{}, tjucli.ListMeta{}, tjucli.NewFlagError(normErr.Error())
	}
	if len(cursor) > MaxCursorBytes {
		return tjucli.ListResult{}, tjucli.ListMeta{}, tjucli.NewFlagError("cursor is too long")
	}

	reqBody := map[string]string{
		"path":   normPath,
		"cursor": cursor,
	}
	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return tjucli.ListResult{}, tjucli.ListMeta{}, tjucli.NewRuntimeError("protocol_error", "failed to encode request")
	}

	var envelope listResponseEnvelope
	cliErr := c.doJSON(ctx, "/v1/course/list", jsonBytes, &envelope)
	if cliErr != nil {
		return tjucli.ListResult{}, tjucli.ListMeta{}, cliErr
	}

	result, meta, valErr := validateListResponse(&envelope)
	if valErr != nil {
		return tjucli.ListResult{}, tjucli.ListMeta{}, valErr
	}

	return result, meta, nil
}

// Search implements courseProvider.Search.
func (c *Client) Search(ctx context.Context, query string, maxPages, limit int) (tjucli.SearchResult, tjucli.SearchMeta, *tjucli.CLIError) {
	if !utf8.ValidString(query) || strings.TrimSpace(query) == "" {
		return tjucli.SearchResult{}, tjucli.SearchMeta{}, tjucli.NewFlagError("search query must not be empty")
	}
	if utf8.RuneCountInString(query) > 256 {
		return tjucli.SearchResult{}, tjucli.SearchMeta{}, tjucli.NewFlagError("search query must not exceed 256 Unicode characters")
	}
	if maxPages < 1 || maxPages > tjucli.MaximumSearchPages {
		return tjucli.SearchResult{}, tjucli.SearchMeta{}, tjucli.NewFlagError(fmt.Sprintf("max-pages must be between 1 and %d", tjucli.MaximumSearchPages))
	}
	if limit < 1 || limit > tjucli.MaximumSearchLimit {
		return tjucli.SearchResult{}, tjucli.SearchMeta{}, tjucli.NewFlagError(fmt.Sprintf("limit must be between 1 and %d", tjucli.MaximumSearchLimit))
	}

	reqBody := struct {
		Query    string `json:"query"`
		MaxPages int    `json:"max_pages"`
		Limit    int    `json:"limit"`
	}{
		Query:    query,
		MaxPages: maxPages,
		Limit:    limit,
	}
	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return tjucli.SearchResult{}, tjucli.SearchMeta{}, tjucli.NewRuntimeError("protocol_error", "failed to encode request")
	}

	var envelope searchResponseEnvelope
	cliErr := c.doJSON(ctx, "/v1/course/search", jsonBytes, &envelope)
	if cliErr != nil {
		return tjucli.SearchResult{}, tjucli.SearchMeta{}, cliErr
	}

	result, meta, valErr := validateSearchResponse(&envelope)
	if valErr != nil {
		return tjucli.SearchResult{}, tjucli.SearchMeta{}, valErr
	}

	return result, meta, nil
}

// Download implements courseProvider.Download.
// Validates path, output path confinement in cwd via os.Root, stream size, SHA256 digest,
// and ensures atomic publication with no overwrite.
func (c *Client) Download(ctx context.Context, providerPath, outputPath string, maxBytes int64) (tjucli.DownloadResult, *tjucli.CLIError) {
	normPath, normErr := normalizeProviderPath(providerPath)
	if normErr != nil {
		return tjucli.DownloadResult{}, tjucli.NewFlagError(normErr.Error())
	}
	if normPath == "/" {
		return tjucli.DownloadResult{}, tjucli.NewFlagError("download path must identify a file")
	}
	if outputPath == "" {
		return tjucli.DownloadResult{}, tjucli.NewFlagError("output path must not be empty")
	}
	if maxBytes < 1 || maxBytes > MaxDownloadBytes {
		return tjucli.DownloadResult{}, tjucli.NewFlagError(fmt.Sprintf("max-bytes must be between 1 and %d", MaxDownloadBytes))
	}

	cwd, err := os.Getwd()
	if err != nil {
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("filesystem_error", "could not determine working directory")
	}

	root, err := os.OpenRoot(cwd)
	if err != nil {
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("filesystem_error", "could not open sandbox root")
	}
	defer root.Close()

	// Clean and normalize target relative path
	cleanOutput := filepath.Clean(outputPath)
	if filepath.IsAbs(cleanOutput) {
		rel, relErr := filepath.Rel(cwd, cleanOutput)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return tjucli.DownloadResult{}, tjucli.NewRuntimeError("target_outside_workspace", "output target must be within working directory")
		}
		cleanOutput = rel
	}
	if cleanOutput == "." || cleanOutput == ".." || strings.HasPrefix(cleanOutput, ".."+string(filepath.Separator)) {
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("target_outside_workspace", "output target must be within working directory")
	}

	// Check if target file already exists in root
	if _, err := root.Lstat(cleanOutput); err == nil {
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("target_exists", "output target already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		// Could be path traversal rejection or filesystem error
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("filesystem_error", "could not inspect output target")
	}

	// Ensure target directory exists in root with private permissions (0700)
	parentDir := filepath.Dir(cleanOutput)
	if parentDir != "." {
		if err := root.MkdirAll(parentDir, 0700); err != nil {
			return tjucli.DownloadResult{}, tjucli.NewRuntimeError("filesystem_error", "could not create target directory")
		}
	}

	// Create temporary file in the target directory within root using cryptographically random suffix and 0600 permissions
	var randSuffix [8]byte
	if _, err := io.ReadFull(rand.Reader, randSuffix[:]); err != nil {
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("filesystem_error", "could not generate temporary filename")
	}
	tempName := filepath.Join(parentDir, fmt.Sprintf(".tjucli-dl-%x", randSuffix))
	tempFile, err := root.OpenFile(tempName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("filesystem_error", "could not create temporary output file")
	}

	published := false
	defer func() {
		_ = tempFile.Close()
		if !published {
			_ = root.Remove(tempName)
		}
	}()

	// Send POST /v1/course/download request
	reqBody := struct {
		Path     string `json:"path"`
		MaxBytes int64  `json:"max_bytes"`
	}{
		Path:     normPath,
		MaxBytes: maxBytes,
	}
	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("protocol_error", "failed to encode download request")
	}
	if len(jsonBytes) > MaxJSONRequestBytes {
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("protocol_error", "request payload exceeds size limit")
	}

	reqURL := c.endpoint("/v1/course/download")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(string(jsonBytes)))
	if err != nil {
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("protocol_error", "failed to create download request")
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Accept", "application/octet-stream")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("download_failed", "course download request failed")
	}
	defer resp.Body.Close()

	if isRedirect(resp.StatusCode) {
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("protocol_error", "server attempted redirect")
	}

	if resp.StatusCode != http.StatusOK {
		return tjucli.DownloadResult{}, parseErrorResponse(resp)
	}

	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/octet-stream" {
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("protocol_error", "server returned unexpected Content-Type")
	}

	expectedSHA := strings.TrimSpace(resp.Header.Get("X-Content-SHA256"))
	if !isLowerHex(expectedSHA, 64) {
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("protocol_error", "invalid X-Content-SHA256 header")
	}

	if resp.ContentLength < 0 {
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("protocol_error", "server omitted Content-Length")
	}
	if resp.ContentLength > maxBytes {
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("size_limit_exceeded", "download exceeds max-bytes")
	}

	hasher := sha256.New()
	multiWriter := io.MultiWriter(tempFile, hasher)

	written, copyErr := io.Copy(multiWriter, io.LimitReader(resp.Body, maxBytes+1))
	if copyErr != nil {
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("download_failed", "course download was interrupted")
	}
	if written > maxBytes {
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("size_limit_exceeded", "download exceeds max-bytes")
	}
	if written != resp.ContentLength {
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("download_failed", "course download length did not match declared length")
	}

	actualSHA := hex.EncodeToString(hasher.Sum(nil))
	if actualSHA != expectedSHA {
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("protocol_error", "checksum mismatch")
	}

	if err := tempFile.Sync(); err != nil {
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("filesystem_error", "could not flush downloaded file")
	}
	if err := tempFile.Close(); err != nil {
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("filesystem_error", "could not close downloaded file")
	}

	// Check context cancellation before publishing
	if err := ctx.Err(); err != nil {
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("download_failed", "download cancelled before publication")
	}

	// Atomic publication via root.Link; fail closed without overwrite.
	// Check if target already exists before linking.
	if _, err := root.Lstat(cleanOutput); err == nil {
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("target_exists", "output target already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("filesystem_error", "could not inspect output target")
	}

	linkErr := root.Link(tempName, cleanOutput)
	if linkErr != nil {
		if errors.Is(linkErr, os.ErrExist) {
			return tjucli.DownloadResult{}, tjucli.NewRuntimeError("target_exists", "output target already exists")
		}
		// If cleanOutput now exists, map to target_exists
		if _, err := root.Lstat(cleanOutput); err == nil {
			return tjucli.DownloadResult{}, tjucli.NewRuntimeError("target_exists", "output target already exists")
		}
		return tjucli.DownloadResult{}, tjucli.NewRuntimeError("filesystem_error", "could not publish downloaded file")
	}

	// Target is published; remove temporary hardlink entry
	_ = root.Remove(tempName)
	published = true

	// Resolve full path for result
	absResult := filepath.Join(cwd, cleanOutput)
	return tjucli.DownloadResult{
		LocalPath: absResult,
		Bytes:     written,
		SHA256:    actualSHA,
	}, nil
}

func (c *Client) doJSON(ctx context.Context, endpointPath string, reqData []byte, out any) *tjucli.CLIError {
	if len(reqData) > MaxJSONRequestBytes {
		return tjucli.NewRuntimeError("protocol_error", "request payload exceeds size limit")
	}

	reqURL := c.endpoint(endpointPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(string(reqData)))
	if err != nil {
		return tjucli.NewRuntimeError("protocol_error", "failed to create request")
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return tjucli.NewRuntimeError("upstream_error", "course provider request failed")
	}
	defer resp.Body.Close()

	if isRedirect(resp.StatusCode) {
		return tjucli.NewRuntimeError("protocol_error", "server attempted redirect")
	}

	if resp.StatusCode != http.StatusOK {
		return parseErrorResponse(resp)
	}

	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return tjucli.NewRuntimeError("protocol_error", "server returned non-JSON response")
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, MaxJSONResponseBytes+1))
	if err != nil {
		return tjucli.NewRuntimeError("upstream_error", "failed to read response")
	}
	if int64(len(respBody)) > MaxJSONResponseBytes {
		return tjucli.NewRuntimeError("protocol_error", "server response exceeds limit")
	}
	if !utf8.Valid(respBody) {
		return tjucli.NewRuntimeError("protocol_error", "server returned invalid UTF-8")
	}

	dec := json.NewDecoder(strings.NewReader(string(respBody)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return tjucli.NewRuntimeError("protocol_error", "failed to decode server response")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return tjucli.NewRuntimeError("protocol_error", "server response contains trailing data")
	}

	return nil
}

type listResponseEnvelope struct {
	OK   *bool             `json:"ok"`
	Data *listResultData   `json:"data"`
	Meta *listMetaEnvelope `json:"meta"`
}

type listResultData struct {
	Items *[]itemData `json:"items"`
}

type listMetaEnvelope struct {
	NextCursor *string `json:"next_cursor"`
}

type searchResponseEnvelope struct {
	OK   *bool               `json:"ok"`
	Data *searchResultData   `json:"data"`
	Meta *searchMetaEnvelope `json:"meta"`
}

type searchResultData struct {
	Items *[]itemData `json:"items"`
}

type searchMetaEnvelope struct {
	Scope        *string `json:"scope"`
	PagesScanned *int    `json:"pages_scanned"`
	Incomplete   *bool   `json:"incomplete"`
}

type itemData struct {
	Name       *string          `json:"name"`
	Path       *string          `json:"path"`
	Kind       *tjucli.ItemKind `json:"kind"`
	Size       *int64           `json:"size"`
	ModifiedAt *string          `json:"modified_at"`
}

func validateItem(item itemData) (tjucli.Item, *tjucli.CLIError) {
	if item.Name == nil || item.Path == nil || item.Kind == nil || item.Size == nil || item.ModifiedAt == nil {
		return tjucli.Item{}, tjucli.NewRuntimeError("protocol_error", "missing required fields in item")
	}
	if *item.Kind != tjucli.KindFile && *item.Kind != tjucli.KindFolder {
		return tjucli.Item{}, tjucli.NewRuntimeError("protocol_error", "invalid item kind")
	}
	normPath, err := normalizeProviderPath(*item.Path)
	if err != nil {
		return tjucli.Item{}, tjucli.NewRuntimeError("protocol_error", "invalid item path")
	}
	if *item.Size < 0 {
		return tjucli.Item{}, tjucli.NewRuntimeError("protocol_error", "negative item size")
	}
	return tjucli.Item{
		Name:       *item.Name,
		Path:       normPath,
		Kind:       *item.Kind,
		Size:       *item.Size,
		ModifiedAt: *item.ModifiedAt,
	}, nil
}

func validateListResponse(env *listResponseEnvelope) (tjucli.ListResult, tjucli.ListMeta, *tjucli.CLIError) {
	if env == nil || env.OK == nil || !*env.OK {
		return tjucli.ListResult{}, tjucli.ListMeta{}, tjucli.NewRuntimeError("protocol_error", "malformed response envelope")
	}
	if env.Data == nil || env.Data.Items == nil {
		return tjucli.ListResult{}, tjucli.ListMeta{}, tjucli.NewRuntimeError("protocol_error", "missing data or items in list response")
	}
	if env.Meta == nil {
		return tjucli.ListResult{}, tjucli.ListMeta{}, tjucli.NewRuntimeError("protocol_error", "missing meta in list response")
	}
	if env.Meta.NextCursor != nil && len(*env.Meta.NextCursor) > MaxCursorBytes {
		return tjucli.ListResult{}, tjucli.ListMeta{}, tjucli.NewRuntimeError("protocol_error", "next_cursor exceeds maximum length")
	}

	items := make([]tjucli.Item, 0, len(*env.Data.Items))
	for _, rawItem := range *env.Data.Items {
		item, err := validateItem(rawItem)
		if err != nil {
			return tjucli.ListResult{}, tjucli.ListMeta{}, err
		}
		items = append(items, item)
	}

	return tjucli.ListResult{Items: items}, tjucli.ListMeta{NextCursor: env.Meta.NextCursor}, nil
}

func validateSearchResponse(env *searchResponseEnvelope) (tjucli.SearchResult, tjucli.SearchMeta, *tjucli.CLIError) {
	if env == nil || env.OK == nil || !*env.OK {
		return tjucli.SearchResult{}, tjucli.SearchMeta{}, tjucli.NewRuntimeError("protocol_error", "malformed response envelope")
	}
	if env.Data == nil || env.Data.Items == nil {
		return tjucli.SearchResult{}, tjucli.SearchMeta{}, tjucli.NewRuntimeError("protocol_error", "missing data or items in search response")
	}
	if env.Meta == nil {
		return tjucli.SearchResult{}, tjucli.SearchMeta{}, tjucli.NewRuntimeError("protocol_error", "missing meta in search response")
	}
	if env.Meta.Scope == nil || *env.Meta.Scope == "" {
		return tjucli.SearchResult{}, tjucli.SearchMeta{}, tjucli.NewRuntimeError("protocol_error", "missing or empty search scope")
	}
	if env.Meta.PagesScanned == nil || *env.Meta.PagesScanned < 0 {
		return tjucli.SearchResult{}, tjucli.SearchMeta{}, tjucli.NewRuntimeError("protocol_error", "missing or negative pages_scanned")
	}
	if env.Meta.Incomplete == nil {
		return tjucli.SearchResult{}, tjucli.SearchMeta{}, tjucli.NewRuntimeError("protocol_error", "missing incomplete flag in search response")
	}

	items := make([]tjucli.Item, 0, len(*env.Data.Items))
	for _, rawItem := range *env.Data.Items {
		item, err := validateItem(rawItem)
		if err != nil {
			return tjucli.SearchResult{}, tjucli.SearchMeta{}, err
		}
		items = append(items, item)
	}

	return tjucli.SearchResult{Items: items}, tjucli.SearchMeta{
		Scope:        *env.Meta.Scope,
		PagesScanned: *env.Meta.PagesScanned,
		Incomplete:   *env.Meta.Incomplete,
	}, nil
}

// Safe static error message mappings for known machine codes.
// Any server message not matching an allowlisted static message is discarded
// to prevent reflecting secrets, tokens, internal URLs, or filesystem paths.
func parseErrorResponse(resp *http.Response) *tjucli.CLIError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	if utf8.Valid(body) {
		var failEnv tjucli.FailureEnvelope
		dec := json.NewDecoder(strings.NewReader(string(body)))
		if dec.Decode(&failEnv) == nil && !failEnv.OK && failEnv.Error.Code != "" {
			switch failEnv.Error.Code {
			case "invalid_argument":
				return tjucli.NewFlagError("invalid argument provided to course service")
			case "unauthorized":
				return tjucli.NewRuntimeError("unauthorized", "unauthorized request")
			case "forbidden":
				return tjucli.NewRuntimeError("forbidden", "forbidden request")
			case "not_found":
				return tjucli.NewRuntimeError("not_found", "requested course resource not found")
			case "target_exists":
				return tjucli.NewRuntimeError("target_exists", "output target already exists")
			case "target_outside_workspace":
				return tjucli.NewRuntimeError("target_outside_workspace", "output target must be within working directory")
			case "file_too_large", "size_limit_exceeded":
				return tjucli.NewRuntimeError("size_limit_exceeded", "download exceeds size limit")
			case "rate_limited", "busy":
				return tjucli.NewRuntimeError("rate_limited", "course service is busy")
			case "upstream_error":
				return tjucli.NewRuntimeError("upstream_error", "upstream provider error")
			case "service_unavailable":
				return tjucli.NewRuntimeError("upstream_error", "course service is unavailable")
			case "protocol_error":
				return tjucli.NewRuntimeError("protocol_error", "course service protocol error")
			default:
				// Unrecognized code mapped to generic safe error
				return tjucli.NewRuntimeError("upstream_error", "course service returned an error")
			}
		}
	}

	switch resp.StatusCode {
	case http.StatusBadRequest:
		return tjucli.NewFlagError("invalid request to course service")
	case http.StatusUnauthorized:
		return tjucli.NewRuntimeError("unauthorized", "unauthorized request")
	case http.StatusForbidden:
		return tjucli.NewRuntimeError("forbidden", "forbidden request")
	case http.StatusNotFound:
		return tjucli.NewRuntimeError("not_found", "requested course resource not found")
	case http.StatusTooManyRequests:
		return tjucli.NewRuntimeError("rate_limited", "course service is busy")
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		return tjucli.NewRuntimeError("upstream_error", "course service unavailable")
	default:
		return tjucli.NewRuntimeError("upstream_error", "course service request failed")
	}
}

func (c *Client) endpoint(p string) string {
	res := *c.cfg.BaseURL
	res.Path = p
	return res.String()
}

func isRedirect(status int) bool {
	return status >= 300 && status <= 399
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

func normalizeProviderPath(value string) (string, error) {
	if value == "" {
		value = "/"
	}
	if len(value) > 4096 {
		return "", errors.New("provider path is too long")
	}
	if !utf8.ValidString(value) {
		return "", errors.New("provider path must be valid UTF-8")
	}
	if strings.ContainsRune(value, '\\') {
		return "", errors.New("provider path must not contain backslashes")
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return "", errors.New("provider path must not contain control characters")
		}
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "." || segment == ".." {
			return "", errors.New("provider path must not contain traversal segments")
		}
	}
	normalized := path.Clean("/" + strings.TrimPrefix(value, "/"))
	return normalized, nil
}
