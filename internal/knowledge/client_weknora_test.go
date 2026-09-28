package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestWeKnoraSearchUsesRealEndpointAndOriginalCitation(t *testing.T) {
	hash := strings.Repeat("a", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost ||
			r.URL.Path != "/api/v1/knowledge-bases/kb-1/hybrid-search" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Error("missing WeKnora API key")
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request["query_text"] != "校历" || request["match_count"] != float64(12) ||
			request["skip_context_enrichment"] != true {
			t.Errorf("unexpected search request fields")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": []map[string]any{{
				"id": "chunk-1", "knowledge_id": "knowledge-1",
				"knowledge_title": "display-only-title", "content": "quoted snippet",
				"score": 0.92, "metadata": map[string]any{
					"source": "campus-calendar", "item_id": "original-item",
					"source_url":     "https://example.test/calendar",
					"canonical_hash": hash,
				},
			}},
		})
	}))
	defer server.Close()
	t.Setenv("WEKNORA_BASE_URL", server.URL)
	t.Setenv("WEKNORA_API_KEY", "test-key")
	t.Setenv("WEKNORA_KNOWLEDGE_BASE_ID", "kb-1")
	t.Setenv("WEKNORA_SEARCH_PATH", "")
	config, cliErr := LoadConfig()
	if cliErr != nil {
		t.Fatal(cliErr)
	}
	client, err := NewClient(config, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	result, cliErr := client.Search(context.Background(), "校历", 3, "")
	if cliErr != nil {
		t.Fatal(cliErr)
	}
	if len(result.Hits) != 1 || result.Hits[0].ChunkID != "chunk-1" ||
		result.Hits[0].CanonicalContentHash != hash ||
		result.Hits[0].ItemID != "original-item" ||
		result.Hits[0].SourceURL != "https://example.test/calendar" {
		t.Fatalf("unexpected WeKnora citation: %#v", result)
	}
}

func TestDefaultWeKnoraEndpointRequiresKnowledgeBaseID(t *testing.T) {
	t.Setenv("WEKNORA_BASE_URL", "https://example.test")
	t.Setenv("WEKNORA_API_KEY", "test-key")
	t.Setenv("WEKNORA_KNOWLEDGE_BASE_ID", "")
	t.Setenv("WEKNORA_SEARCH_PATH", "")
	if _, cliErr := LoadConfig(); cliErr == nil || cliErr.Code != "configuration_error" {
		t.Fatalf("missing knowledge base should be rejected: %#v", cliErr)
	}
	if _, err := NewClient(Config{
		BaseURL: "https://example.test", APIKey: "test-key",
		SearchPath: DefaultSearchPath,
	}, nil); err == nil {
		t.Fatal("programmatic WeKnora client accepted missing knowledge base")
	}
}

func TestWeKnoraSearchRejectsMissingCitationInsteadOfFabricatingOne(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"data":[{"id":"chunk-1","knowledge_id":"knowledge-1","knowledge_title":"title","content":"snippet","score":0.8,"metadata":{}}]}`))
	}))
	defer server.Close()
	client, err := NewClient(Config{
		BaseURL: server.URL, APIKey: "test-key",
		KnowledgeBaseID: "kb-1", SearchPath: DefaultSearchPath,
	}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, cliErr := client.Search(context.Background(), "校历", 3, "")
	if cliErr == nil || cliErr.Code != "protocol_error" {
		t.Fatalf("expected missing provenance error, got %#v", cliErr)
	}
}

func TestWeKnoraSearchFiltersSourceAndBoundsResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "total": 4,
				"data": []map[string]any{
					{"id": "doc-other", "knowledge_base_id": "kb-1", "parse_status": "completed", "metadata": map[string]any{"source": "other"}},
					{"id": "doc-calendar-1", "knowledge_base_id": "kb-1", "parse_status": "completed", "metadata": map[string]any{"source": "calendar"}},
					{"id": "doc-calendar-2", "knowledge_base_id": "kb-1", "parse_status": "completed", "metadata": map[string]any{"source": "calendar"}},
					{"id": "doc-calendar-3", "knowledge_base_id": "kb-1", "parse_status": "failed", "metadata": map[string]any{"source": "calendar"}},
				}})
			return
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		ids, ok := request["knowledge_ids"].([]any)
		if !ok || len(ids) != 2 || ids[0] != "doc-calendar-1" || ids[1] != "doc-calendar-2" ||
			request["match_count"] != float64(8) {
			t.Errorf("unexpected source scope: %#v", request)
		}
		if _, ok := request["top_k"]; ok {
			t.Error("WeKnora v0.8.0 does not document a top_k request field")
		}
		data := make([]map[string]any, 0, 2)
		for index := range 2 {
			data = append(data, map[string]any{
				"id": "chunk-" + string(rune('1'+index)), "knowledge_id": "doc-calendar-" + string(rune('1'+index)),
				"content": "quotation", "score": 0.8,
				"metadata": map[string]any{
					"source": "calendar", "item_id": "item", "source_url": "https://example.test/",
					"canonical_hash": strings.Repeat("a", 64),
				},
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
	}))
	defer server.Close()
	client, err := NewClient(Config{
		BaseURL: server.URL, APIKey: "test-key",
		KnowledgeBaseID: "kb-1", SearchPath: DefaultSearchPath,
	}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	result, cliErr := client.Search(context.Background(), "校历", 2, "calendar")
	if cliErr != nil {
		t.Fatal(cliErr)
	}
	if len(result.Hits) != 2 || result.Hits[0].Source != "calendar" ||
		result.Hits[0].ChunkID != "chunk-1" || result.Hits[1].ChunkID != "chunk-2" {
		t.Fatalf("unexpected filtered WeKnora hits: %#v", result)
	}
}

func TestWeKnoraScopedSearchFindsSourceBeyondFirstListingPage(t *testing.T) {
	const kb = "kb-source-scope"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			page := r.URL.Query().Get("page")
			rows := make([]map[string]any, 0, knowledgePageSize)
			if page == "1" {
				for index := range knowledgePageSize {
					rows = append(rows, map[string]any{"id": "other-" + strings.Repeat("0", 8) + string(rune('a'+index%26)),
						"knowledge_base_id": kb, "parse_status": "completed", "metadata": map[string]any{"source": "other"}})
				}
			} else if page == "2" {
				rows = append(rows, map[string]any{"id": "target-knowledge", "knowledge_base_id": kb,
					"parse_status": "completed", "metadata": map[string]any{"source": "campus-calendar"}})
			} else {
				t.Errorf("unexpected page %s", page)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "total": knowledgePageSize + 1, "data": rows})
			return
		}
		var request struct {
			KnowledgeIDs []string `json:"knowledge_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil ||
			len(request.KnowledgeIDs) != 1 || request.KnowledgeIDs[0] != "target-knowledge" {
			t.Errorf("source scope missing from search: %+v, %v", request, err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": []map[string]any{{
			"id": "chunk-target", "knowledge_id": "target-knowledge",
			"content": "synthetic snippet", "score": 0.8,
			"metadata": map[string]any{"source": "campus-calendar", "item_id": "synthetic",
				"source_url": "https://example.test/calendar", "canonical_hash": strings.Repeat("a", 64)},
		}}})
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, APIKey: "test-key",
		KnowledgeBaseID: kb, SearchPath: DefaultSearchPath}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	result, cliErr := client.Search(context.Background(), "校历", 10, "campus-calendar")
	if cliErr != nil || len(result.Hits) != 1 || result.Hits[0].KnowledgeID != "target-knowledge" {
		t.Fatalf("source-scoped retrieval failed: %d hits, error %v", len(result.Hits), cliErr)
	}
}

func TestWeKnoraScopedSearchAcceptsLargeForumScopeWithoutDroppingDocuments(t *testing.T) {
	const total = 56_000
	const kb = "kb-large-forum"
	lastID := fmt.Sprintf("%036d", total-1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if r.URL.Query().Get("page_size") != strconv.Itoa(knowledgePageSize) {
				t.Error("listing did not use verified larger page size")
			}
			page, err := strconv.Atoi(r.URL.Query().Get("page"))
			if err != nil {
				t.Error(err)
			}
			start := (page - 1) * knowledgePageSize
			rows := make([]map[string]any, 0, knowledgePageSize)
			for n := start; n < start+knowledgePageSize && n < total; n++ {
				rows = append(rows, map[string]any{
					"id": fmt.Sprintf("%036d", n), "knowledge_base_id": kb,
					"parse_status": "completed", "metadata": map[string]any{"source": "wepeiyang-lake-posts"},
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "total": total, "data": rows})
			return
		}
		var payload struct {
			KnowledgeIDs []string `json:"knowledge_ids"`
			MatchCount   int      `json:"match_count"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil ||
			len(payload.KnowledgeIDs) != total || payload.KnowledgeIDs[total-1] != lastID ||
			payload.MatchCount != 4 {
			t.Errorf("incomplete large source scope: ids=%d error=%v", len(payload.KnowledgeIDs), err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": []map[string]any{{
			"id": "chunk-last", "knowledge_id": lastID,
			"content": "synthetic quotation", "score": 0.8,
			"metadata": map[string]any{"source": "wepeiyang-lake-posts", "item_id": "synthetic",
				"source_url": "https://example.test/forum", "canonical_hash": strings.Repeat("a", 64)},
		}}})
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, APIKey: "test-key",
		KnowledgeBaseID: kb, SearchPath: DefaultSearchPath}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	result, cliErr := client.Search(context.Background(), "校园", 1, "wepeiyang-lake-posts")
	if cliErr != nil || len(result.Hits) != 1 || result.Hits[0].KnowledgeID != lastID {
		t.Fatalf("large scoped search failed: %d hits, error %v", len(result.Hits), cliErr)
	}
}

func TestWeKnoraSearchFusesVectorScoreWithKeywordOnlyHit(t *testing.T) {
	const kb = "kb-fuse"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			DisableKeywordsMatch bool    `json:"disable_keywords_match"`
			DisableVectorMatch   bool    `json:"disable_vector_match"`
			VectorThreshold      float64 `json:"vector_threshold"`
			MatchCount           int     `json:"match_count"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.MatchCount != 12 || request.DisableKeywordsMatch == request.DisableVectorMatch {
			t.Errorf("channels were not requested separately: %+v", request)
		}
		hash := strings.Repeat("b", 64)
		meta := map[string]any{
			"source": "twt-studyroom-catalog", "item_id": "room",
			"source_url": "https://example.test/room", "canonical_hash": hash,
		}
		if request.DisableKeywordsMatch {
			if request.VectorThreshold != 0.2 {
				t.Errorf("vector threshold = %v", request.VectorThreshold)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": []map[string]any{{
				"id": "chunk-vector", "knowledge_id": "knowledge-vector",
				"content": "vector quotation", "score": 0.81, "metadata": meta,
			}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": []map[string]any{
			{
				"id": "chunk-vector", "knowledge_id": "knowledge-vector",
				"content": "vector quotation", "score": 37.5, "metadata": meta,
			},
			{
				"id": "chunk-keyword", "knowledge_id": "knowledge-keyword",
				"content": "keyword quotation", "score": 12, "metadata": map[string]any{
					"source": "peiyang-wiki-public", "item_id": "wiki",
					"source_url": "https://example.test/wiki", "canonical_hash": hash,
				},
			},
		}})
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, APIKey: "test-key",
		KnowledgeBaseID: kb, SearchPath: DefaultSearchPath}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	result, cliErr := client.Search(context.Background(), "自习室开放时间", 3, "")
	if cliErr != nil {
		t.Fatal(cliErr)
	}
	if len(result.Hits) != 2 {
		t.Fatalf("hits = %#v", result.Hits)
	}
	if result.Hits[0].ChunkID != "chunk-vector" || result.Hits[0].Score < 0.81 || result.Hits[0].Score > 1 {
		t.Fatalf("vector hit was not kept as the stronger citation: %#v", result.Hits[0])
	}
	if result.Hits[1].ChunkID != "chunk-keyword" || result.Hits[1].Score <= 0 || result.Hits[1].Score > 1 {
		t.Fatalf("keyword-only hit was not calibrated: %#v", result.Hits[1])
	}
}

func TestWeKnoraSourceListingContinuesWhenTotalGrows(t *testing.T) {
	const kb = "kb-growing"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			page := r.URL.Query().Get("page")
			if page == "1" {
				rows := make([]map[string]any, knowledgePageSize)
				for index := range rows {
					rows[index] = map[string]any{
						"id": fmt.Sprintf("other-%08d", index), "knowledge_base_id": kb,
						"parse_status": "completed", "metadata": map[string]any{"source": "other"},
					}
				}
				rows[0] = map[string]any{
					"id": "target-knowledge", "knowledge_base_id": kb,
					"parse_status": "completed", "metadata": map[string]any{"source": "campus-calendar"},
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "total": knowledgePageSize + 1, "data": rows})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "total": knowledgePageSize + 2, "data": []map[string]any{
				{"id": "target-knowledge", "knowledge_base_id": kb, "parse_status": "completed",
					"metadata": map[string]any{"source": "campus-calendar"}},
				{"id": "added-knowledge", "knowledge_base_id": kb, "parse_status": "completed",
					"metadata": map[string]any{"source": "campus-calendar"}},
			}})
			return
		}
		var request struct {
			KnowledgeIDs []string `json:"knowledge_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if len(request.KnowledgeIDs) != 2 || request.KnowledgeIDs[0] != "target-knowledge" ||
			request.KnowledgeIDs[1] != "added-knowledge" {
			t.Errorf("growing list was not deduped: %#v", request.KnowledgeIDs)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": []map[string]any{{
			"id": "chunk-target", "knowledge_id": "target-knowledge",
			"content": "synthetic snippet", "score": 0.8,
			"metadata": map[string]any{"source": "campus-calendar", "item_id": "synthetic",
				"source_url": "https://example.test/calendar", "canonical_hash": strings.Repeat("c", 64)},
		}}})
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, APIKey: "test-key",
		KnowledgeBaseID: kb, SearchPath: DefaultSearchPath}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	result, cliErr := client.Search(context.Background(), "校历", 1, "campus-calendar")
	if cliErr != nil || len(result.Hits) != 1 || result.Hits[0].KnowledgeID != "target-knowledge" {
		t.Fatalf("search during a growing source list failed: %d hits, error %v", len(result.Hits), cliErr)
	}
}

func TestDiversifyCampusHitsLeadsWithDistinctDocuments(t *testing.T) {
	hits := []Hit{
		{Source: "peiyang-wiki-public", ItemID: "hours", ChunkID: "hours-1", Score: 0.90, QuotedText: "07:00"},
		{Source: "peiyang-wiki-public", ItemID: "hours", ChunkID: "hours-2", Score: 0.80, QuotedText: "same page"},
		{Source: "twt-studyroom-catalog", ItemID: "room", ChunkID: "room-1", Score: 0.75, QuotedText: "room"},
	}
	got := diversifyCampusHits(hits, 2)
	if len(got) != 2 || got[0].ChunkID != "hours-1" || got[1].ChunkID != "room-1" {
		t.Fatalf("distinct documents were not preferred: %#v", got)
	}
	filled := diversifyCampusHits(hits, 3)
	if len(filled) != 3 || filled[2].ChunkID != "hours-2" {
		t.Fatalf("extra chunk was not used to fill the limit: %#v", filled)
	}
}

func TestFuseCampusHitsPrefersOfficialPageWithinScoreMargin(t *testing.T) {
	vector := []Hit{
		{Source: "wepeiyang-lake-posts", ItemID: "post", KnowledgeID: "k-post", ChunkID: "post-1", Score: 0.667, QuotedText: "forum"},
		{Source: "peiyang-wiki-public", ItemID: "calendar", KnowledgeID: "k-wiki", ChunkID: "wiki-1", Score: 0.649, QuotedText: "wiki"},
	}
	got := fuseCampusHits(vector, nil, 2, "校历")
	if len(got) != 2 || got[0].ChunkID != "wiki-1" || got[0].Score != 0.649 {
		t.Fatalf("official page was not preferred without rewriting its score: %#v", got)
	}
	vector[0].Score = 0.80
	got = fuseCampusHits(vector, nil, 2, "校历")
	if len(got) != 2 || got[0].ChunkID != "post-1" {
		t.Fatalf("clearly stronger forum hit was buried: %#v", got)
	}
}

func TestFuseCampusHitsLiftsStudyroomCatalogForRoomQueries(t *testing.T) {
	vector := []Hit{
		{Source: "wepeiyang-lake-posts", ItemID: "post", KnowledgeID: "k-post", ChunkID: "post-1", Score: 0.688, QuotedText: "forum"},
		{Source: "twt-studyroom-catalog", ItemID: "room", KnowledgeID: "k-room", ChunkID: "room-1", Score: 0.588, QuotedText: "room"},
	}
	got := fuseCampusHits(vector, nil, 2, "空教室")
	if len(got) != 2 || got[0].ChunkID != "room-1" || got[0].Score != 0.588 {
		t.Fatalf("studyroom catalog was not preferred for an empty-room query: %#v", got)
	}
	plain := fuseCampusHits(vector, nil, 2, "食堂")
	if len(plain) != 2 || plain[0].ChunkID != "post-1" {
		t.Fatalf("studyroom catalog was boosted for an unrelated query: %#v", plain)
	}
}
