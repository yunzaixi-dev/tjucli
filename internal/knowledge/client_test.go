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
