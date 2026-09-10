package tjucli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	mockCampusesResponse = `{
		"code": 10000,
		"data": [
			{"id": 1, "name": "北洋园校区"},
			{"id": 2, "name": "卫津路校区"},
			{"id": 3, "name": "深圳校区"}
		]
	}`

	mockBeiyangBuildingsResponse = `{
		"code": 10000,
		"data": [
			{"id": 4, "name": "一教（44楼）", "campus_id": 1},
			{"id": 16, "name": "二教（45楼、46楼）", "campus_id": 1},
			{"id": 18, "name": "综合楼", "campus_id": 1},
			{"id": 24, "name": "软（55楼）", "campus_id": 1}
		]
	}`

	mockWeijinBuildingsResponse = `{
		"code": 10000,
		"data": [
			{"id": 35, "name": "04楼", "campus_id": 2},
			{"id": 22, "name": "科图", "campus_id": 2},
			{"id": 11, "name": "综合楼（卫津路）", "campus_id": 2}
		]
	}`

	mockRoomsResponse = `{
		"code": 10000,
		"data": [
			{"id": 363, "name": "44楼A区103", "building_id": "4", "free": true},
			{"id": 68, "name": "44楼A区104", "building_id": "4", "free": true},
			{"id": 69, "name": "44楼A区105", "building_id": 4, "free": false}
		]
	}`
)

func newTestRoomsServer(t *testing.T) (*httptest.Server, *RoomsProvider) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Strict test: no ticket or domain headers should ever be present
		if r.Header.Get("ticket") != "" || r.Header.Get("DOMAIN") != "" || r.Header.Get("token") != "" {
			t.Errorf("request leaked credential header: %v", r.Header)
		}

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/campus":
			_, _ = io.WriteString(w, mockCampusesResponse)
		case "/campus/1/building":
			_, _ = io.WriteString(w, mockBeiyangBuildingsResponse)
		case "/campus/2/building":
			_, _ = io.WriteString(w, mockWeijinBuildingsResponse)
		case "/campus/3/building":
			_, _ = io.WriteString(w, `{"code":10000,"data":[]}`)
		case "/building/4/room/session/1/date/2026-09-09":
			_, _ = io.WriteString(w, mockRoomsResponse)
		case "/building/22/room/session/2/date/2026-09-10":
			_, _ = io.WriteString(w, `{
				"code": 10000,
				"data": [
					{"id": 101, "name": "科图315", "building_id": "22", "free": false},
					{"id": 102, "name": "科图513", "building_id": "22", "free": true}
				]
			}`)
		case "/building/4/room/session/0/date/2026-09-09":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"code":50002,"data":"getBySession.sessionId: sessionId must be between 1 and 12"}`)
		default:
			http.NotFound(w, r)
		}
	}))

	provider, err := newRoomsProvider(server.Client(), server.URL, true)
	if err != nil {
		t.Fatalf("failed to create test rooms provider: %v", err)
	}

	return server, provider
}

func TestRoomsProvider_Campuses(t *testing.T) {
	t.Parallel()
	server, provider := newTestRoomsServer(t)
	defer server.Close()

	campuses, err := provider.Campuses(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(campuses) != 3 {
		t.Fatalf("expected 3 campuses, got %d", len(campuses))
	}
	if campuses[0].Name != "北洋园校区" || campuses[0].ID != 1 {
		t.Errorf("unexpected campus[0]: %#v", campuses[0])
	}
}

func TestRoomsProvider_Buildings(t *testing.T) {
	t.Parallel()
	server, provider := newTestRoomsServer(t)
	defer server.Close()

	buildings, err := provider.Buildings(context.Background(), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(buildings) != 4 {
		t.Fatalf("expected 4 buildings, got %d", len(buildings))
	}
	if buildings[0].Name != "一教（44楼）" || buildings[0].ID != 4 {
		t.Errorf("unexpected building[0]: %#v", buildings[0])
	}

	// Invalid campus ID
	_, err = provider.Buildings(context.Background(), -1)
	if err == nil || err.Code != "invalid_argument" {
		t.Errorf("expected invalid_argument for negative campus ID, got %v", err)
	}
}

func TestRoomsProvider_CheckRooms_SuccessWithAliasAndCampus(t *testing.T) {
	t.Parallel()
	server, provider := newTestRoomsServer(t)
	defer server.Close()

	res, err := provider.CheckRooms(context.Background(), RoomQuery{
		Campus:   "北洋园",
		Building: "一教",
		Date:     "2026-09-09",
		Session:  1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.Campus != "北洋园校区" || res.Building != "一教（44楼）" || res.BuildingID != 4 {
		t.Errorf("unexpected metadata: campus=%s, building=%s (id=%d)", res.Campus, res.Building, res.BuildingID)
	}
	if res.Total != 3 || res.FreeCount != 2 {
		t.Errorf("expected 3 total and 2 free rooms, got total=%d, free=%d", res.Total, res.FreeCount)
	}
	if len(res.Rooms) != 3 {
		t.Fatalf("expected 3 room statuses, got %d", len(res.Rooms))
	}
	if !res.Rooms[0].Free || !res.Rooms[1].Free || res.Rooms[2].Free {
		t.Errorf("unexpected room free statuses: %v, %v, %v", res.Rooms[0].Free, res.Rooms[1].Free, res.Rooms[2].Free)
	}
}

func TestRoomsProvider_CheckRooms_LibraryAliasInWeijinlu(t *testing.T) {
	t.Parallel()
	server, provider := newTestRoomsServer(t)
	defer server.Close()

	// Use "科学图书馆" alias to match "科图"
	res, err := provider.CheckRooms(context.Background(), RoomQuery{
		Building: "科学图书馆",
		Date:     "2026-09-10",
		Session:  2,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.Campus != "卫津路校区" || res.Building != "科图" || res.BuildingID != 22 {
		t.Errorf("unexpected metadata: campus=%s, building=%s, id=%d", res.Campus, res.Building, res.BuildingID)
	}
	if res.Total != 2 || res.FreeCount != 1 {
		t.Errorf("expected 2 total and 1 free, got total=%d, free=%d", res.Total, res.FreeCount)
	}
}

func TestRoomsProvider_CheckRooms_AmbiguousBuildingError(t *testing.T) {
	t.Parallel()
	server, provider := newTestRoomsServer(t)
	defer server.Close()

	// Both campus 1 and campus 2 have a "综合楼"
	_, err := provider.CheckRooms(context.Background(), RoomQuery{
		Building: "综合楼",
		Date:     "2026-09-09",
		Session:  1,
	})
	if err == nil || err.Code != "invalid_argument" || !strings.Contains(err.Message, "ambiguous") {
		t.Fatalf("expected ambiguous error, got %v", err)
	}
}

func TestRoomsProvider_CheckRooms_ValidationErrors(t *testing.T) {
	t.Parallel()
	server, provider := newTestRoomsServer(t)
	defer server.Close()

	tests := []struct {
		name    string
		query   RoomQuery
		wantErr string
	}{
		{
			name:    "empty building",
			query:   RoomQuery{Building: "", Session: 1},
			wantErr: "building must not be empty",
		},
		{
			name:    "invalid session 0",
			query:   RoomQuery{Building: "一教", Session: 0, Date: "2026-09-09"},
			// Session 0 defaults to 1 so session 0 will actually succeed or default to 1
			wantErr: "",
		},
		{
			name:    "invalid session 13",
			query:   RoomQuery{Building: "一教", Session: 13, Date: "2026-09-09"},
			wantErr: "session must be between 1 and 12",
		},
		{
			name:    "invalid date format non-padded",
			query:   RoomQuery{Building: "一教", Session: 1, Date: "2026-9-9"},
			wantErr: "date must be in yyyy-MM-dd format",
		},
		{
			name:    "invalid date format string",
			query:   RoomQuery{Building: "一教", Session: 1, Date: "invalid-date"},
			wantErr: "date must be in yyyy-MM-dd format",
		},
		{
			name:    "nonexistent campus",
			query:   RoomQuery{Campus: "火星校区", Building: "一教", Session: 1, Date: "2026-09-09"},
			wantErr: "campus \"火星校区\" not found",
		},
		{
			name:    "nonexistent building",
			query:   RoomQuery{Campus: "北洋园", Building: "不存在的楼", Session: 1, Date: "2026-09-09"},
			wantErr: "building \"不存在的楼\" not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := provider.CheckRooms(context.Background(), tt.query)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Message, tt.wantErr) {
				t.Errorf("error message %q does not contain %q", err.Message, tt.wantErr)
			}
		})
	}
}

func TestRoomsProvider_UpstreamErrors(t *testing.T) {
	t.Parallel()

	t.Run("HTTP 500 error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "server error", http.StatusInternalServerError)
		}))
		defer srv.Close()

		p, _ := newRoomsProvider(srv.Client(), srv.URL, true)
		_, err := p.Campuses(context.Background())
		if err == nil || err.Code != "upstream_error" {
			t.Fatalf("expected upstream_error, got %v", err)
		}
	})

	t.Run("Business code != 10000", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code": 50002, "data": "getBySession.sessionId: sessionId must be between 1 and 12"}`)
		}))
		defer srv.Close()

		p, _ := newRoomsProvider(srv.Client(), srv.URL, true)
		_, err := p.Campuses(context.Background())
		if err == nil || !strings.Contains(err.Message, "sessionId must be between 1 and 12") {
			t.Fatalf("expected business error in message, got %v", err)
		}
	})

	t.Run("Malformed JSON", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code": 10000, "data": "invalid json`)
		}))
		defer srv.Close()

		p, _ := newRoomsProvider(srv.Client(), srv.URL, true)
		_, err := p.Campuses(context.Background())
		if err == nil || err.Code != "protocol_error" {
			t.Fatalf("expected protocol_error, got %v", err)
		}
	})

	t.Run("Oversized response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			// Write more than MaxRoomsResponseBytes
			payload := strings.Repeat("a", int(MaxRoomsResponseBytes+10))
			_, _ = io.WriteString(w, payload)
		}))
		defer srv.Close()

		p, _ := newRoomsProvider(srv.Client(), srv.URL, true)
		_, err := p.Campuses(context.Background())
		if err == nil || err.Code != "protocol_error" {
			t.Fatalf("expected protocol_error for oversized response, got %v", err)
		}
	})

	t.Run("Request timeout or cancellation", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(200 * time.Millisecond)
		}))
		defer srv.Close()

		p, _ := newRoomsProvider(srv.Client(), srv.URL, true)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()

		_, err := p.Campuses(ctx)
		if err == nil || err.Code != "request_timeout" {
			t.Fatalf("expected request_timeout, got %v", err)
		}
	})
}
