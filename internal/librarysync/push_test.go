package librarysync

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yunzaixi-dev/tjucli/internal/account"
)

// fakeLibrary is just enough of the API for push: list, read, create.
type fakeLibrary struct {
	mu      sync.Mutex
	entries []Entry
	creates []string
	failing map[string]int // title -> status to answer with, once
}

func (f *fakeLibrary) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/entries"):
		_ = json.NewEncoder(w).Encode(map[string]any{"entries": f.entries})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/entries/"):
		for _, e := range f.entries {
			if e.ID == strings.TrimPrefix(r.URL.Path, "/entries/") {
				_ = json.NewEncoder(w).Encode(map[string]any{"entry": e})
				return
			}
		}
		w.WriteHeader(404)
	case r.Method == "POST":
		title := ""
		if strings.HasSuffix(r.URL.Path, "/files") {
			_ = r.ParseMultipartForm(1 << 20)
			_, header, _ := r.FormFile("file")
			title = header.Filename
		} else {
			var body struct{ Title string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			title = body.Title
		}
		if status := f.failing[title]; status != 0 {
			delete(f.failing, title)
			w.WriteHeader(status) // an edge error page, not the API's JSON
			_, _ = w.Write([]byte("<html>gateway</html>"))
			return
		}
		f.creates = append(f.creates, title)
		e := Entry{ID: "new-" + title, Kind: KindNote, Title: title, UpdatedAt: "t1"}
		if strings.HasSuffix(r.URL.Path, "/files") {
			e.Kind = KindFile
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"entry": e})
	default:
		w.WriteHeader(405)
	}
}

func pushCopy(t *testing.T, fake *fakeLibrary, files map[string]string) *Copy {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	root := t.TempDir()
	for p, body := range files {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755)
		if err := os.WriteFile(filepath.Join(root, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &Copy{Root: root, client: account.NewClient(account.Credentials{API: server.URL, Token: "tjc_x"}),
		State: State{Version: stateVersion, API: server.URL, LibraryID: "lib", Entries: map[string]Tracked{}}}
}

func TestPushAdoptsWhatAnEarlierPushCreatedWithoutAReply(t *testing.T) {
	fake := &fakeLibrary{entries: []Entry{
		{ID: "saved-file", Kind: KindFile, Title: "图.png", Size: 5, UpdatedAt: "t0"},
		{ID: "saved-note", Kind: KindNote, Title: "笔记", Body: "same text", UpdatedAt: "t0"},
		{ID: "other-note", Kind: KindNote, Title: "另一篇", Body: "different", UpdatedAt: "t0"},
	}}
	c := pushCopy(t, fake, map[string]string{"图.png": "12345", "笔记.md": "same text", "另一篇.md": "mine"})
	res, err := c.Push(t.Context(), false, nil)
	if err != nil || len(res.Failed) != 0 {
		t.Fatalf("push: %v %+v", err, res)
	}
	if strings.Join(fake.creates, ",") != "另一篇" {
		t.Fatalf("created %v; only the note whose text differs should be sent", fake.creates)
	}
	if c.State.Entries["图.png"].ID != "saved-file" || c.State.Entries["笔记.md"].ID != "saved-note" {
		t.Fatalf("not adopted: %+v", c.State.Entries)
	}
}

func TestPushReportsAnEdgeErrorAndGoesOn(t *testing.T) {
	fake := &fakeLibrary{failing: map[string]int{"坏": 502}}
	c := pushCopy(t, fake, map[string]string{"坏.md": "x", "好.md": "y"})
	res, err := c.Push(t.Context(), false, nil)
	if err != nil {
		t.Fatalf("push stopped: %v", err)
	}
	if len(res.Failed) != 1 || res.Failed[0].Path != "坏.md" || res.Failed[0].Error != "http_502" {
		t.Fatalf("failed: %+v", res.Failed)
	}
	if _, ok := c.State.Entries["好.md"]; !ok {
		t.Fatal("the other note was not pushed")
	}
	// The failed one is still new, and goes on the next push.
	res, err = c.Push(t.Context(), false, nil)
	if err != nil || len(res.Created) != 1 || res.Created[0] != "坏.md" {
		t.Fatalf("retry: %v %+v", err, res)
	}
}
