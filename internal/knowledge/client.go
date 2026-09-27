package knowledge

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yunzaixi-dev/tjucli/internal/tjucli"
)

const (
	DefaultSearchPath  = "/api/v1/knowledge-search"
	DefaultSearchLimit = 50
	MaxQueryBytes      = 8 * 1024
	MaxRequestBytes    = 4 * 1024 * 1024
	MaxResponseBytes   = 4 * 1024 * 1024
	MaxSearchLimit     = 100
	knowledgePageSize  = 1000
)

type Config struct {
	BaseURL         string
	APIKey          string
	KnowledgeBaseID string
	SearchPath      string
}
type Client struct {
	baseURL         *url.URL
	apiKey          string
	knowledgeBaseID string
	searchPath      string
	httpClient      *http.Client
}
type SearchResult struct {
	Hits []Hit `json:"hits"`
}
type Hit struct {
	Source               string  `json:"source"`
	ItemID               string  `json:"item_id"`
	SourceURL            string  `json:"source_url"`
	CanonicalContentHash string  `json:"canonical_content_hash"`
	KnowledgeID          string  `json:"knowledge_id"`
	ChunkID              string  `json:"chunk_id"`
	QuotedText           string  `json:"quoted_text"`
	Score                float64 `json:"score"`
	Category             string  `json:"category,omitempty"`
}

func LoadConfig() (Config, *tjucli.CLIError) {
	config := Config{
		BaseURL:         strings.TrimSpace(os.Getenv("WEKNORA_BASE_URL")),
		APIKey:          os.Getenv("WEKNORA_API_KEY"),
		KnowledgeBaseID: strings.TrimSpace(os.Getenv("WEKNORA_KNOWLEDGE_BASE_ID")),
		SearchPath:      strings.TrimSpace(os.Getenv("WEKNORA_SEARCH_PATH")),
	}
	if config.SearchPath == "" {
		config.SearchPath = DefaultSearchPath
	}
	if config.BaseURL == "" || config.APIKey == "" {
		return Config{}, tjucli.NewRuntimeError("configuration_error", "WEKNORA_BASE_URL and WEKNORA_API_KEY are required")
	}
	if config.SearchPath == DefaultSearchPath && config.KnowledgeBaseID == "" {
		return Config{}, tjucli.NewRuntimeError("configuration_error", "WEKNORA_KNOWLEDGE_BASE_ID is required for WeKnora search")
	}
	parsed, err := url.Parse(config.BaseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopback(parsed.Hostname()))) {
		return Config{}, tjucli.NewRuntimeError("configuration_error", "WEKNORA_BASE_URL must use HTTPS unless loopback")
	}
	return config, nil
}

func NewClient(config Config, httpClient *http.Client) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(config.BaseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("invalid WeKnora base URL")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopback(parsed.Hostname())) {
		return nil, errors.New("WeKnora base URL must use HTTPS unless loopback")
	}
	if config.APIKey == "" {
		return nil, errors.New("WeKnora API key is required")
	}
	path, err := cleanSearchPath(config.SearchPath)
	if err != nil {
		return nil, err
	}
	if path == DefaultSearchPath && strings.TrimSpace(config.KnowledgeBaseID) == "" {
		return nil, errors.New("WeKnora knowledge base ID is required")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	} else {
		clone := *httpClient
		if clone.Timeout <= 0 || clone.Timeout > 10*time.Second {
			clone.Timeout = 10 * time.Second
		}
		httpClient = &clone
	}
	return &Client{baseURL: parsed, apiKey: config.APIKey, knowledgeBaseID: config.KnowledgeBaseID, searchPath: path, httpClient: httpClient}, nil
}

func (c *Client) Search(ctx context.Context, query string, limit int, source string) (SearchResult, *tjucli.CLIError) {
	if strings.TrimSpace(query) == "" || len([]byte(query)) > MaxQueryBytes {
		return SearchResult{}, tjucli.NewFlagError("knowledge query must be non-empty and at most 8192 bytes")
	}
	if limit < 1 || limit > MaxSearchLimit {
		return SearchResult{}, tjucli.NewFlagError("knowledge limit must be between 1 and 100")
	}
	var payload any
	target := *c.baseURL
	var scopedIDs map[string]struct{}
	if c.searchPath == DefaultSearchPath && c.knowledgeBaseID != "" {
		// The generic knowledge-search endpoint returns a fixed ten results,
		// regardless of the CLI limit. Hybrid search accepts match_count and
		// preserves citation metadata on each returned chunk.
		var knowledgeIDs []string
		if source != "" {
			var listErr *tjucli.CLIError
			knowledgeIDs, listErr = c.sourceKnowledgeIDs(ctx, source)
			if listErr != nil {
				return SearchResult{}, listErr
			}
			if len(knowledgeIDs) == 0 {
				return SearchResult{Hits: []Hit{}}, nil
			}
			scopedIDs = make(map[string]struct{}, len(knowledgeIDs))
			for _, id := range knowledgeIDs {
				scopedIDs[id] = struct{}{}
			}
		}
		target.Path = strings.TrimRight(target.Path, "/") + "/api/v1/knowledge-bases/" +
			url.PathEscape(c.knowledgeBaseID) + "/hybrid-search"
		payload = struct {
			QueryText             string   `json:"query_text"`
			MatchCount            int      `json:"match_count"`
			SkipContextEnrichment bool     `json:"skip_context_enrichment"`
			KnowledgeIDs          []string `json:"knowledge_ids,omitempty"`
		}{query, limit, true, knowledgeIDs}
	} else {
		target.Path = strings.TrimRight(target.Path, "/") + c.searchPath
		payload = struct {
			Query  string `json:"query"`
			Limit  int    `json:"limit"`
			Source string `json:"source,omitempty"`
		}{query, limit, source}
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil || len(payloadBytes) > MaxRequestBytes {
		return SearchResult{}, tjucli.NewRuntimeError("upstream_error", "knowledge source has too many documents for one search")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(payloadBytes))
	if err != nil {
		return SearchResult{}, tjucli.NewRuntimeError("upstream_error", "knowledge search request failed")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("X-API-Key", c.apiKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return SearchResult{}, tjucli.NewRuntimeError("upstream_error", "knowledge search request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return SearchResult{}, tjucli.NewRuntimeError("upstream_error", "knowledge search upstream returned an unexpected status")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil || len(body) > MaxResponseBytes {
		return SearchResult{}, tjucli.NewRuntimeError("protocol_error", "knowledge search response is too large")
	}
	var raw struct {
		Hits    *[]json.RawMessage `json:"hits"`
		Data    *[]json.RawMessage `json:"data"`
		Success *bool              `json:"success"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return SearchResult{}, tjucli.NewRuntimeError("protocol_error", "unsupported knowledge search response")
	}
	if raw.Success != nil && !*raw.Success {
		return SearchResult{}, tjucli.NewRuntimeError("upstream_error", "knowledge search upstream rejected the request")
	}
	hits := raw.Hits
	if hits == nil {
		hits = raw.Data
	}
	maxHits := limit
	if raw.Data != nil {
		maxHits = MaxSearchLimit
	}
	if hits == nil || len(*hits) > maxHits {
		return SearchResult{}, tjucli.NewRuntimeError("protocol_error", "unsupported knowledge search response")
	}
	result := SearchResult{Hits: make([]Hit, 0, len(*hits))}
	for _, rawHit := range *hits {
		var hit Hit
		if raw.Data != nil {
			hit, err = normalizeWeKnoraHit(rawHit)
		} else {
			err = json.Unmarshal(rawHit, &hit)
		}
		if err != nil {
			return SearchResult{}, tjucli.NewRuntimeError("protocol_error", "unsupported knowledge search hit")
		}
		if err := validateHit(hit); err != nil {
			return SearchResult{}, tjucli.NewRuntimeError("protocol_error", err.Error())
		}
		if scopedIDs != nil {
			if _, ok := scopedIDs[hit.KnowledgeID]; !ok || hit.Source != source {
				return SearchResult{}, tjucli.NewRuntimeError("protocol_error", "knowledge search returned an out-of-scope hit")
			}
		}
		result.Hits = append(result.Hits, hit)
		if len(result.Hits) >= limit {
			break
		}
	}
	return result, nil
}

func (c *Client) sourceKnowledgeIDs(ctx context.Context, source string) ([]string, *tjucli.CLIError) {
	for attempt := 0; attempt < 3; attempt++ {
		var total int
		knownTotal := false
		ids := make([]string, 0)
		seen := make(map[string]struct{})
		retry := false
		for page := 1; page <= 100; page++ {
			target := *c.baseURL
			target.Path = strings.TrimRight(target.Path, "/") + "/api/v1/knowledge-bases/" +
				url.PathEscape(c.knowledgeBaseID) + "/knowledge"
			target.RawQuery = "page=" + strconv.Itoa(page) + "&page_size=" + strconv.Itoa(knowledgePageSize)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
			if err != nil {
				return nil, tjucli.NewRuntimeError("upstream_error", "knowledge source listing failed")
			}
			req.Header.Set("X-API-Key", c.apiKey)
			resp, err := c.httpClient.Do(req)
			if err != nil {
				return nil, tjucli.NewRuntimeError("upstream_error", "knowledge source listing failed")
			}
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || readErr != nil || len(body) > MaxResponseBytes {
				return nil, tjucli.NewRuntimeError("upstream_error", "knowledge source listing failed")
			}
			var listing struct {
				Success *bool `json:"success"`
				Total   *int  `json:"total"`
				Data    []struct {
					ID              string         `json:"id"`
					KnowledgeBaseID string         `json:"knowledge_base_id"`
					ParseStatus     string         `json:"parse_status"`
					Metadata        map[string]any `json:"metadata"`
				} `json:"data"`
			}
			if json.Unmarshal(body, &listing) != nil || listing.Success == nil || !*listing.Success ||
				listing.Total == nil || *listing.Total < 0 || *listing.Total > 100000 ||
				listing.Data == nil || len(listing.Data) > knowledgePageSize {
				return nil, tjucli.NewRuntimeError("protocol_error", "invalid knowledge source listing")
			}
			if knownTotal && total != *listing.Total {
				retry = true
				break
			}
			total, knownTotal = *listing.Total, true
			for _, row := range listing.Data {
				if row.KnowledgeBaseID != c.knowledgeBaseID {
					return nil, tjucli.NewRuntimeError("protocol_error", "knowledge source listing crossed knowledge bases")
				}
				if row.ParseStatus != "completed" || row.Metadata["source"] != source {
					continue
				}
				if len(row.ID) < 8 || len(row.ID) > 128 {
					return nil, tjucli.NewRuntimeError("protocol_error", "invalid knowledge source identifier")
				}
				for _, character := range row.ID {
					if !(character >= 'a' && character <= 'z' ||
						character >= 'A' && character <= 'Z' ||
						character >= '0' && character <= '9' || character == '-') {
						return nil, tjucli.NewRuntimeError("protocol_error", "invalid knowledge source identifier")
					}
				}
				if _, duplicate := seen[row.ID]; duplicate {
					return nil, tjucli.NewRuntimeError("protocol_error", "duplicate knowledge source identifier")
				}
				seen[row.ID] = struct{}{}
				ids = append(ids, row.ID)
			}
			if page*knowledgePageSize >= total {
				return ids, nil
			}
			if len(listing.Data) != knowledgePageSize {
				return nil, tjucli.NewRuntimeError("protocol_error", "incomplete knowledge source listing")
			}
		}
		if !retry {
			return nil, tjucli.NewRuntimeError("protocol_error", "knowledge source listing exceeds limit")
		}
	}
	return nil, tjucli.NewRuntimeError("upstream_error", "knowledge source changed during listing")
}

func normalizeWeKnoraHit(raw json.RawMessage) (Hit, error) {
	var item struct {
		ID          string         `json:"id"`
		KnowledgeID string         `json:"knowledge_id"`
		ChunkID     string         `json:"chunk_id"`
		Content     string         `json:"content"`
		Score       float64        `json:"score"`
		Metadata    map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return Hit{}, err
	}
	if item.ID == "" || item.KnowledgeID == "" {
		return Hit{}, errors.New("missing WeKnora identifiers")
	}
	field := func(name string) string {
		value, _ := item.Metadata[name].(string)
		return value
	}
	chunkID := item.ChunkID
	if chunkID == "" {
		chunkID = item.ID
	}
	hash := field("canonical_content_hash")
	if hash == "" {
		hash = field("canonical_hash")
	}
	return Hit{
		Source: field("source"), ItemID: field("item_id"), SourceURL: field("source_url"),
		CanonicalContentHash: hash, KnowledgeID: item.KnowledgeID,
		ChunkID: chunkID, QuotedText: item.Content, Score: item.Score,
		Category: field("category"),
	}, nil
}

func validateHit(hit Hit) error {
	if hit.Source == "" || hit.ItemID == "" || hit.SourceURL == "" || hit.CanonicalContentHash == "" || hit.KnowledgeID == "" || hit.ChunkID == "" || hit.QuotedText == "" {
		return errors.New("knowledge hit is missing citation provenance")
	}
	parsed, err := url.Parse(hit.SourceURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return errors.New("knowledge hit has invalid source URL")
	}
	if len(hit.CanonicalContentHash) != 64 {
		return errors.New("knowledge hit has invalid canonical content hash")
	}
	if _, err := hex.DecodeString(hit.CanonicalContentHash); err != nil || strings.ToLower(hit.CanonicalContentHash) != hit.CanonicalContentHash {
		return errors.New("knowledge hit has invalid canonical content hash")
	}
	if hit.Score < 0 || hit.Score > 1 || math.IsNaN(hit.Score) || math.IsInf(hit.Score, 0) {
		return errors.New("knowledge hit has invalid score")
	}
	return nil
}

func ValidateSearchResult(result SearchResult, limit int) error {
	if limit < 1 || limit > MaxSearchLimit || len(result.Hits) > limit {
		return errors.New("knowledge search result exceeds limit")
	}
	for _, hit := range result.Hits {
		if err := validateHit(hit); err != nil {
			return err
		}
	}
	return nil
}

func cleanSearchPath(path string) (string, error) {
	if path == "" || !strings.HasPrefix(path, "/") || strings.Contains(path, "?") || strings.Contains(path, "#") || strings.Contains(path, "..") {
		return "", errors.New("invalid WeKnora search path")
	}
	return path, nil
}
func isLoopback(host string) bool { return host == "localhost" || host == "127.0.0.1" || host == "::1" }
