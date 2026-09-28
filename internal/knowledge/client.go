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
	"sort"
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
	if c.searchPath == DefaultSearchPath && c.knowledgeBaseID != "" {
		return c.searchWeKnora(ctx, query, limit, source)
	}
	target := *c.baseURL
	target.Path = strings.TrimRight(target.Path, "/") + c.searchPath
	payload := struct {
		Query  string `json:"query"`
		Limit  int    `json:"limit"`
		Source string `json:"source,omitempty"`
	}{query, limit, source}
	hits, cliErr := c.postHits(ctx, target.String(), payload, false, false, nil)
	if cliErr != nil {
		return SearchResult{}, cliErr
	}
	if len(hits) > limit {
		return SearchResult{}, tjucli.NewRuntimeError("protocol_error", "unsupported knowledge search response")
	}
	return SearchResult{Hits: hits}, nil
}

// searchWeKnora asks for vector and keyword channels separately. WeKnora's
// combined hybrid response replaces both with a reciprocal-rank score near
// zero, which hides a relevant campus passage from anything that trusts score.
func (c *Client) searchWeKnora(ctx context.Context, query string, limit int, source string) (SearchResult, *tjucli.CLIError) {
	var knowledgeIDs []string
	var scopedIDs map[string]struct{}
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
	fetch := weknoraFetchCount(limit)
	vector, cliErr := c.weknoraChannel(ctx, query, fetch, knowledgeIDs, scopedIDs, source, true)
	if cliErr != nil {
		return SearchResult{}, cliErr
	}
	keyword, keywordErr := c.weknoraChannel(ctx, query, fetch, knowledgeIDs, scopedIDs, source, false)
	if keywordErr != nil {
		if keywordErr.Code != "upstream_error" {
			return SearchResult{}, keywordErr
		}
		keyword = nil
	}
	return SearchResult{Hits: fuseCampusHits(vector, keyword, limit, query)}, nil
}

func (c *Client) weknoraChannel(ctx context.Context, query string, limit int, knowledgeIDs []string, scopedIDs map[string]struct{}, source string, vector bool) ([]Hit, *tjucli.CLIError) {
	target := *c.baseURL
	target.Path = strings.TrimRight(target.Path, "/") + "/api/v1/knowledge-bases/" +
		url.PathEscape(c.knowledgeBaseID) + "/hybrid-search"
	payload := struct {
		QueryText             string   `json:"query_text"`
		MatchCount            int      `json:"match_count"`
		SkipContextEnrichment bool     `json:"skip_context_enrichment"`
		KnowledgeIDs          []string `json:"knowledge_ids,omitempty"`
		DisableKeywordsMatch  bool     `json:"disable_keywords_match,omitempty"`
		DisableVectorMatch    bool     `json:"disable_vector_match,omitempty"`
		VectorThreshold       float64  `json:"vector_threshold,omitempty"`
	}{
		QueryText: query, MatchCount: limit, SkipContextEnrichment: true, KnowledgeIDs: knowledgeIDs,
		DisableKeywordsMatch: vector, DisableVectorMatch: !vector,
	}
	if vector {
		payload.VectorThreshold = 0.2
	}
	return c.postHits(ctx, target.String(), payload, true, !vector, func(hit Hit) *tjucli.CLIError {
		if err := validateHit(hit); err != nil {
			return tjucli.NewRuntimeError("protocol_error", err.Error())
		}
		if scopedIDs != nil {
			if _, ok := scopedIDs[hit.KnowledgeID]; !ok || hit.Source != source {
				return tjucli.NewRuntimeError("protocol_error", "knowledge search returned an out-of-scope hit")
			}
		}
		return nil
	})
}

func (c *Client) postHits(ctx context.Context, target string, payload any, weknora bool, keyword bool, accept func(Hit) *tjucli.CLIError) ([]Hit, *tjucli.CLIError) {
	payloadBytes, err := json.Marshal(payload)
	if err != nil || len(payloadBytes) > MaxRequestBytes {
		return nil, tjucli.NewRuntimeError("upstream_error", "knowledge source has too many documents for one search")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, tjucli.NewRuntimeError("upstream_error", "knowledge search request failed")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("X-API-Key", c.apiKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, tjucli.NewRuntimeError("upstream_error", "knowledge search request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, tjucli.NewRuntimeError("upstream_error", "knowledge search upstream returned an unexpected status")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil || len(body) > MaxResponseBytes {
		return nil, tjucli.NewRuntimeError("protocol_error", "knowledge search response is too large")
	}
	var raw struct {
		Hits    *[]json.RawMessage `json:"hits"`
		Data    *[]json.RawMessage `json:"data"`
		Success *bool              `json:"success"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, tjucli.NewRuntimeError("protocol_error", "unsupported knowledge search response")
	}
	if raw.Success != nil && !*raw.Success {
		return nil, tjucli.NewRuntimeError("upstream_error", "knowledge search upstream rejected the request")
	}
	hits := raw.Hits
	if weknora {
		hits = raw.Data
	}
	maxHits := MaxSearchLimit
	if !weknora {
		maxHits = 0
	}
	if hits == nil || (weknora && len(*hits) > maxHits) || (!weknora && raw.Hits == nil) {
		return nil, tjucli.NewRuntimeError("protocol_error", "unsupported knowledge search response")
	}
	if !weknora && len(*hits) > MaxSearchLimit {
		return nil, tjucli.NewRuntimeError("protocol_error", "unsupported knowledge search response")
	}
	result := make([]Hit, 0, len(*hits))
	for _, rawHit := range *hits {
		var hit Hit
		if weknora {
			hit, err = normalizeWeKnoraHit(rawHit)
		} else {
			err = json.Unmarshal(rawHit, &hit)
		}
		if err != nil {
			return nil, tjucli.NewRuntimeError("protocol_error", "unsupported knowledge search hit")
		}
		if keyword {
			calibrated, scoreErr := calibrateKeywordScore(hit.Score)
			if scoreErr != nil {
				return nil, tjucli.NewRuntimeError("protocol_error", scoreErr.Error())
			}
			hit.Score = calibrated
		}
		if accept != nil {
			if cliErr := accept(hit); cliErr != nil {
				return nil, cliErr
			}
		} else if err := validateHit(hit); err != nil {
			return nil, tjucli.NewRuntimeError("protocol_error", err.Error())
		}
		result = append(result, hit)
		if accept == nil && len(result) >= MaxSearchLimit {
			break
		}
	}
	return result, nil
}

func calibrateKeywordScore(score float64) (float64, error) {
	if math.IsNaN(score) || math.IsInf(score, 0) || score < 0 {
		return 0, errors.New("knowledge hit has invalid score")
	}
	if score > 1 {
		score = score / (score + 8)
	}
	if score > 1 {
		return 0, errors.New("knowledge hit has invalid score")
	}
	return score, nil
}

func fuseCampusHits(vector, keyword []Hit, limit int, query string) []Hit {
	type scored struct {
		hit     Hit
		vector  float64
		keyword float64
		hasVec  bool
		hasKey  bool
	}
	order := make([]string, 0, len(vector)+len(keyword))
	byKey := make(map[string]*scored, len(vector)+len(keyword))
	add := func(hit Hit, fromVector bool) {
		key := hit.KnowledgeID + "\x00" + hit.ChunkID
		slot, ok := byKey[key]
		if !ok {
			slot = &scored{hit: hit}
			byKey[key] = slot
			order = append(order, key)
		}
		if fromVector {
			slot.hasVec = true
			if hit.Score > slot.vector {
				slot.vector = hit.Score
			}
			return
		}
		slot.hasKey = true
		if hit.Score > slot.keyword {
			slot.keyword = hit.Score
		}
	}
	for _, hit := range vector {
		add(hit, true)
	}
	for _, hit := range keyword {
		add(hit, false)
	}
	hits := make([]Hit, 0, len(order))
	for _, key := range order {
		slot := byKey[key]
		score := slot.keyword
		if slot.hasVec {
			score = slot.vector
			if slot.hasKey {
				if slot.keyword > score {
					score = slot.keyword
				}
				score += 0.03
				if score > 1 {
					score = 1
				}
			}
		}
		slot.hit.Score = score
		hits = append(hits, slot.hit)
	}
	// Official pages and forum posts often land within a few hundredths.
	// Rank key prefers the official page inside that margin. The printed score
	// stays the fused retrieval score.
	sort.SliceStable(hits, func(i, j int) bool {
		left, right := campusRank(hits[i], query), campusRank(hits[j], query)
		if left == right {
			return hits[i].Score > hits[j].Score
		}
		return left > right
	})
	return diversifyCampusHits(hits, limit)
}

const officialSourceBoost = 0.04

func officialCampusSource(source string) bool {
	switch source {
	case "peiyang-wiki-public", "twt-studyroom-catalog", "campus-calendar":
		return true
	default:
		return strings.HasPrefix(source, "college-")
	}
}

func campusRank(hit Hit, query string) float64 {
	score := hit.Score
	if officialCampusSource(hit.Source) {
		score += officialSourceBoost
	}
	score += intentSourceBonus(query, hit.Source)
	if score > 1 {
		return 1
	}
	return score
}

// intentSourceBonus lifts the study-room catalog for room-availability
// questions. Forum posts often embed closer to the words "空教室" than the
// catalog's room rows, but the catalog is the operational answer. The printed
// score is unchanged.
func intentSourceBonus(query, source string) float64 {
	if source != "twt-studyroom-catalog" {
		return 0
	}
	for _, term := range []string{"空教室", "空闲教室", "自习室", "机房"} {
		if strings.Contains(query, term) {
			return 0.08
		}
	}
	return 0
}

// weknoraFetchCount asks for more chunks than the caller wants so one long
// document cannot fill the channel before a more specific campus page appears.
func weknoraFetchCount(limit int) int {
	fetch := limit * 4
	if fetch > MaxSearchLimit {
		return MaxSearchLimit
	}
	return fetch
}

// diversifyCampusHits keeps the best chunk of each document first, then fills
// any remaining slots with later chunks. Callers that ask for many hits still
// receive them; a short result is not four slices of the same page.
func diversifyCampusHits(hits []Hit, limit int) []Hit {
	if len(hits) == 0 || limit < 1 {
		return []Hit{}
	}
	seen := make(map[string]struct{}, len(hits))
	primary := make([]Hit, 0, len(hits))
	extra := make([]Hit, 0)
	for _, hit := range hits {
		key := hit.Source + "\x00" + hit.ItemID
		if _, ok := seen[key]; ok {
			extra = append(extra, hit)
			continue
		}
		seen[key] = struct{}{}
		primary = append(primary, hit)
	}
	combined := append(primary, extra...)
	if len(combined) > limit {
		combined = combined[:limit]
	}
	return combined
}

func (c *Client) sourceKnowledgeIDs(ctx context.Context, source string) ([]string, *tjucli.CLIError) {
	for attempt := 0; attempt < 8; attempt++ {
		ids := make([]string, 0)
		seen := make(map[string]struct{})
		var count int
		known := false
		shrunk := false
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
			total := *listing.Total
			grew := known && total > count
			if known && total < count {
				shrunk = true
				break
			}
			count, known = total, true
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
					if !grew {
						return nil, tjucli.NewRuntimeError("protocol_error", "duplicate knowledge source identifier")
					}
					continue
				}
				seen[row.ID] = struct{}{}
				ids = append(ids, row.ID)
			}
			if page*knowledgePageSize >= count {
				return ids, nil
			}
			if len(listing.Data) != knowledgePageSize {
				return nil, tjucli.NewRuntimeError("protocol_error", "incomplete knowledge source listing")
			}
		}
		if !shrunk {
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
