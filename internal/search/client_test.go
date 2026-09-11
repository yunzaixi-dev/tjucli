package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type mockMaterial struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

func TestCliMeiliClient(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /indexes", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})

	mux.HandleFunc("POST /indexes/campus_materials/documents", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(TaskResponse{
			TaskUID:  10,
			IndexUID: "campus_materials",
			Status:   "enqueued",
		})
	})

	mux.HandleFunc("POST /indexes/campus_materials/search", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(SearchResponse[mockMaterial]{
			Hits: []mockMaterial{
				{ID: "m-1", Name: "高等数学期末题", Path: "/math/final.pdf"},
			},
			EstimatedTotalHits: 1,
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	cli, err := NewClient(server.URL, "")
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	ctx := context.Background()

	if err := cli.EnsureIndex(ctx, "campus_materials", "id"); err != nil {
		t.Errorf("EnsureIndex failed: %v", err)
	}

	docs := []mockMaterial{{ID: "m-1", Name: "高等数学期末题", Path: "/math/final.pdf"}}
	task, err := cli.AddDocuments(ctx, "campus_materials", docs)
	if err != nil {
		t.Errorf("AddDocuments failed: %v", err)
	}
	if task == nil || task.TaskUID != 10 {
		t.Errorf("unexpected task: %+v", task)
	}

	resp, err := Search[mockMaterial](ctx, cli, "campus_materials", SearchRequest{Q: "数学"})
	if err != nil {
		t.Errorf("Search failed: %v", err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].ID != "m-1" {
		t.Errorf("unexpected hits: %+v", resp.Hits)
	}
}
