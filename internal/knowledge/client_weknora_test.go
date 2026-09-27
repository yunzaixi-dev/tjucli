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
		if request["query_text"] != "校历" || request["match_count"] != float64(3) ||
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
			request["match_count"] != float64(2) {
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
			payload.MatchCount != 1 {
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
