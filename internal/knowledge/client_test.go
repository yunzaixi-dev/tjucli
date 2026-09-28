package knowledge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientSearchReturnsCitationBearingHits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/search" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"hits": []map[string]any{{
			"source": "course-notes", "item_id": "item-1", "source_url": "https://example.test/a",
			"canonical_content_hash": strings.Repeat("a", 64), "knowledge_id": "knowledge-1",
			"chunk_id": "chunk-1", "quoted_text": "quoted", "score": 0.91, "category": "course",
		}}})
	}))
	defer server.Close()

	client, err := NewClient(Config{BaseURL: server.URL, APIKey: "test-key", SearchPath: "/search"}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	result, cliErr := client.Search(context.Background(), "数学", 3, "course")
	if cliErr != nil {
		t.Fatalf("search failed: %v", cliErr)
	}
	if len(result.Hits) != 1 || result.Hits[0].ItemID != "item-1" || result.Hits[0].Category != "course" {
		t.Fatalf("result = %#v", result)
	}
}

func TestWeKnoraSearchRequestsEnoughCitedHitsBeyondDefaultTen(t *testing.T) {
	const kb = "knowledge-base-test"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			rows := make([]map[string]any, 25)
			for index := range rows {
				rows[index] = map[string]any{"id": "knowledge-id-" + string(rune('a'+index)),
					"knowledge_base_id": kb, "parse_status": "completed",
					"metadata": map[string]any{"source": "public-course-sharing"}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "total": len(rows), "data": rows})
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/knowledge-bases/"+kb+"/hybrid-search" {
			t.Errorf("unexpected WeKnora endpoint: %s %s", r.Method, r.URL.Path)
			http.Error(w, "wrong endpoint", http.StatusNotFound)
			return
		}
		var payload struct {
			QueryText             string   `json:"query_text"`
			MatchCount            int      `json:"match_count"`
			SkipContextEnrichment bool     `json:"skip_context_enrichment"`
			KnowledgeIDs          []string `json:"knowledge_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.QueryText != "校园信息" ||
			payload.MatchCount != 100 || !payload.SkipContextEnrichment || len(payload.KnowledgeIDs) != 25 {
			t.Errorf("unexpected WeKnora request: %+v, %v", payload, err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		hits := make([]map[string]any, 25)
		for index := range hits {
			hits[index] = map[string]any{
				"id": "chunk-id", "knowledge_id": payload.KnowledgeIDs[index],
				"content": "quoted", "score": 0.9,
				"metadata": map[string]any{
					"source": "public-course-sharing", "item_id": "item-id",
					"source_url":     "https://example.test/course",
					"canonical_hash": strings.Repeat("a", 64),
				},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": hits})
	}))
	defer server.Close()
	client, err := NewClient(Config{
		BaseURL: server.URL, APIKey: "test-key", KnowledgeBaseID: kb,
		SearchPath: DefaultSearchPath,
	}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	result, cliErr := client.Search(context.Background(), "校园信息", 25, "public-course-sharing")
	if cliErr != nil {
		t.Fatal(cliErr)
	}
	if len(result.Hits) != 25 || result.Hits[0].SourceURL == "" {
		t.Fatalf("missing requested citation-bearing hits: %d", len(result.Hits))
	}
}

func TestClientRejectsMissingProvenanceAndNeverLeaksKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"hits":[{"source":"x","item_id":"i"}]}`))
	}))
	defer server.Close()

	client, err := NewClient(Config{BaseURL: server.URL, APIKey: "secret-key", SearchPath: "/search"}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, cliErr := client.Search(context.Background(), "q", 1, "")
	if cliErr == nil || cliErr.Code != "protocol_error" || strings.Contains(cliErr.Message, "secret-key") {
		t.Fatalf("error = %#v", cliErr)
	}
}

func TestClientRejectsMissingHitsAndTooManyHits(t *testing.T) {
	for _, response := range []string{`{}`, `{"hits":[{},{}]}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(response))
		}))
		client, err := NewClient(Config{BaseURL: server.URL, APIKey: "key", SearchPath: "/search"}, server.Client())
		if err != nil {
			t.Fatal(err)
		}
		_, cliErr := client.Search(context.Background(), "q", 1, "")
		server.Close()
		if cliErr == nil || cliErr.Code != "protocol_error" {
			t.Fatalf("response %q error = %#v", response, cliErr)
		}
	}
}

func TestLoadConfigRequiresCredentialsAndHTTPS(t *testing.T) {
	t.Setenv("WEKNORA_BASE_URL", "http://remote.example")
	t.Setenv("WEKNORA_API_KEY", "secret-key")
	if _, cliErr := LoadConfig(); cliErr == nil || cliErr.Code != "configuration_error" {
		t.Fatalf("expected HTTPS configuration error, got %#v", cliErr)
	}
	t.Setenv("WEKNORA_BASE_URL", "https://remote.example")
	t.Setenv("WEKNORA_API_KEY", "")
	if _, cliErr := LoadConfig(); cliErr == nil || cliErr.Code != "configuration_error" {
		t.Fatalf("expected credential configuration error, got %#v", cliErr)
	}
}
