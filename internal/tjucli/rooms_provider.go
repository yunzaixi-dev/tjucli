package tjucli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var defaultBuildingAliases = map[string]string{
	"一教":    "44",
	"二教":    "45",
	"三教":    "46",
	"四教":    "33",
	"44教":   "44",
	"45教":   "45",
	"46教":   "46",
	"33教":   "33",
	"50教":   "50",
	"55教":   "55",
	"化教":    "50",
	"软教":    "55",
	"科学图书馆": "科图",
	"南馆":    "科图",
}

// RoomsProvider queries room availability from the verified selfstudy API.
type RoomsProvider struct {
	client        *http.Client
	baseURL       *url.URL
	allowInsecure bool
}

// NewRoomsProvider constructs a RoomsProvider targeting the default selfstudy API.
func NewRoomsProvider() (*RoomsProvider, error) {
	return newRoomsProvider(&http.Client{Timeout: DefaultRoomsTimeoutSec * time.Second}, DefaultRoomsBaseURL, false)
}

// newRoomsProvider constructs a RoomsProvider with an injected client and URL.
func newRoomsProvider(client *http.Client, baseURL string, allowInsecure bool) (*RoomsProvider, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("invalid rooms provider base URL")
	}
	if parsed.Scheme != "https" && !(allowInsecure && parsed.Scheme == "http") {
		return nil, errors.New("rooms provider base URL must use HTTPS")
	}
	if client == nil {
		client = &http.Client{}
	}
	clone := *client
	if clone.Timeout <= 0 {
		clone.Timeout = DefaultRoomsTimeoutSec * time.Second
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return &RoomsProvider{
		client:        &clone,
		baseURL:       parsed,
		allowInsecure: allowInsecure,
	}, nil
}

type roomsAPIEnvelope struct {
	Code int             `json:"code"`
	Data json.RawMessage `json:"data"`
}

type upstreamRoomItem struct {
	ID         int             `json:"id"`
	Name       string          `json:"name"`
	BuildingID json.RawMessage `json:"building_id"`
	Free       bool            `json:"free"`
}

func (p *RoomsProvider) doRequest(ctx context.Context, requestPath string) ([]byte, *CLIError) {
	endpoint := *p.baseURL
	endpoint.Path = strings.TrimRight(p.baseURL.Path, "/") + requestPath

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, NewRuntimeError("protocol_error", "could not create rooms provider request")
	}
	// Strictly anonymous: no credential headers, no ticket, no domain header.
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, NewRuntimeError("request_timeout", "rooms provider request timed out or was canceled")
		}
		return nil, NewRuntimeError("upstream_error", "rooms provider request failed")
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, NewRuntimeError("upstream_error", fmt.Sprintf("rooms provider returned HTTP %d", resp.StatusCode))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxRoomsResponseBytes+1))
	if err != nil {
		return nil, NewRuntimeError("upstream_error", "rooms provider response could not be read")
	}
	if int64(len(body)) > MaxRoomsResponseBytes {
		return nil, NewRuntimeError("protocol_error", "rooms provider response is too large")
	}

	var env roomsAPIEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, NewRuntimeError("protocol_error", "rooms provider returned malformed JSON")
	}

	if env.Code != 10000 {
		var errMsg string
		if json.Unmarshal(env.Data, &errMsg) == nil && errMsg != "" {
			return nil, NewRuntimeError("upstream_error", fmt.Sprintf("rooms provider error: %s", errMsg))
		}
		return nil, NewRuntimeError("upstream_error", fmt.Sprintf("rooms provider returned error code %d", env.Code))
	}

	return env.Data, nil
}

// Campuses returns the list of university campuses.
func (p *RoomsProvider) Campuses(ctx context.Context) ([]Campus, *CLIError) {
	data, cliErr := p.doRequest(ctx, "/campus")
	if cliErr != nil {
		return nil, cliErr
	}

	var campuses []Campus
	if err := json.Unmarshal(data, &campuses); err != nil {
		return nil, NewRuntimeError("protocol_error", "rooms provider returned malformed campuses list")
	}

	for _, c := range campuses {
		if c.ID <= 0 || c.Name == "" || !utf8.ValidString(c.Name) {
			return nil, NewRuntimeError("protocol_error", "rooms provider returned invalid campus data")
		}
	}

	return campuses, nil
}

// Buildings returns the list of buildings for a specific campus ID.
func (p *RoomsProvider) Buildings(ctx context.Context, campusID int) ([]Building, *CLIError) {
	if campusID <= 0 {
		return nil, NewFlagError("campus ID must be a positive integer")
	}

	data, cliErr := p.doRequest(ctx, fmt.Sprintf("/campus/%d/building", campusID))
	if cliErr != nil {
		return nil, cliErr
	}

	var buildings []Building
	if err := json.Unmarshal(data, &buildings); err != nil {
		return nil, NewRuntimeError("protocol_error", "rooms provider returned malformed buildings list")
	}

	for _, b := range buildings {
		if b.ID <= 0 || b.Name == "" || !utf8.ValidString(b.Name) {
			return nil, NewRuntimeError("protocol_error", "rooms provider returned invalid building data")
		}
	}

	return buildings, nil
}

// CheckRooms checks the real-time room availability for a given building, date, and session.
func (p *RoomsProvider) CheckRooms(ctx context.Context, query RoomQuery) (RoomQueryResult, *CLIError) {
	if err := validateQueryString(query.Building, "building"); err != nil {
		return RoomQueryResult{}, err
	}
	if query.Campus != "" {
		if err := validateQueryString(query.Campus, "campus"); err != nil {
			return RoomQueryResult{}, err
		}
	}

	date := strings.TrimSpace(query.Date)
	if date == "" {
		date = TodayInShanghai()
	} else {
		validDate, err := ValidateDate(date)
		if err != nil {
			return RoomQueryResult{}, err
		}
		date = validDate
	}

	session := query.Session
	if session == 0 {
		session = 1
	}
	if session < MinSessionIndex || session > MaxSessionIndex {
		return RoomQueryResult{}, NewFlagError(fmt.Sprintf("session must be between %d and %d", MinSessionIndex, MaxSessionIndex))
	}

	// 1. Fetch campuses
	campuses, cliErr := p.Campuses(ctx)
	if cliErr != nil {
		return RoomQueryResult{}, cliErr
	}

	// 2. Resolve Campus and Building
	var matchedCampus *Campus
	var matchedBuilding *Building

	if query.Campus != "" {
		c, err := ResolveCampus(campuses, query.Campus)
		if err != nil {
			return RoomQueryResult{}, err
		}
		matchedCampus = c

		buildings, err := p.Buildings(ctx, matchedCampus.ID)
		if err != nil {
			return RoomQueryResult{}, err
		}

		b, err := ResolveBuilding(buildings, query.Building)
		if err != nil {
			return RoomQueryResult{}, err
		}
		matchedBuilding = b
	} else {
		// Search across all campuses
		type campusBuildingPair struct {
			campus   Campus
			building Building
		}
		var pairs []campusBuildingPair

		for _, c := range campuses {
			// Skip campuses with no rooms known to exist if needed, or query each
			buildings, err := p.Buildings(ctx, c.ID)
			if err != nil {
				continue
			}
			b, err := ResolveBuilding(buildings, query.Building)
			if err == nil && b != nil {
				pairs = append(pairs, campusBuildingPair{campus: c, building: *b})
			}
		}

		if len(pairs) == 0 {
			return RoomQueryResult{}, NewFlagError(fmt.Sprintf("building %q not found", query.Building))
		}
		if len(pairs) > 1 {
			var names []string
			for _, p := range pairs {
				names = append(names, fmt.Sprintf("%s (%s)", p.building.Name, p.campus.Name))
			}
			return RoomQueryResult{}, NewFlagError(fmt.Sprintf("building %q is ambiguous across campuses (%s); please specify --campus", query.Building, strings.Join(names, ", ")))
		}

		matchedCampus = &pairs[0].campus
		matchedBuilding = &pairs[0].building
	}

	// 3. Request room availability
	requestPath := fmt.Sprintf("/building/%d/room/session/%d/date/%s", matchedBuilding.ID, session, date)
	data, cliErr := p.doRequest(ctx, requestPath)
	if cliErr != nil {
		return RoomQueryResult{}, cliErr
	}

	var upstreamRooms []upstreamRoomItem
	if err := json.Unmarshal(data, &upstreamRooms); err != nil {
		return RoomQueryResult{}, NewRuntimeError("protocol_error", "rooms provider returned malformed room status data")
	}

	rooms := make([]RoomStatus, 0, len(upstreamRooms))
	freeCount := 0
	for _, raw := range upstreamRooms {
		bID, err := parseBuildingID(raw.BuildingID)
		if err != nil {
			bID = matchedBuilding.ID
		}
		if raw.Free {
			freeCount++
		}
		rooms = append(rooms, RoomStatus{
			ID:         raw.ID,
			Name:       raw.Name,
			BuildingID: bID,
			Free:       raw.Free,
		})
	}

	return RoomQueryResult{
		Campus:     matchedCampus.Name,
		CampusID:   matchedCampus.ID,
		Building:   matchedBuilding.Name,
		BuildingID: matchedBuilding.ID,
		Date:       date,
		Session:    session,
		Total:      len(rooms),
		FreeCount:  freeCount,
		Rooms:      rooms,
	}, nil
}

// ResolveCampus searches for a campus by exact ID or name substring.
func ResolveCampus(campuses []Campus, query string) (*Campus, *CLIError) {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return nil, NewFlagError("campus must not be empty")
	}

	// Check if integer ID
	if id, err := strconv.Atoi(trimmed); err == nil && id > 0 {
		for i := range campuses {
			if campuses[i].ID == id {
				return &campuses[i], nil
			}
		}
	}

	// Substring / keyword match
	var matches []*Campus
	for i := range campuses {
		if campuses[i].Name == trimmed {
			return &campuses[i], nil
		}
		if strings.Contains(campuses[i].Name, trimmed) || strings.Contains(trimmed, campuses[i].Name) {
			matches = append(matches, &campuses[i])
		}
	}

	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		var names []string
		for _, m := range matches {
			names = append(names, m.Name)
		}
		return nil, NewFlagError(fmt.Sprintf("campus %q is ambiguous (%s)", query, strings.Join(names, ", ")))
	}

	var available []string
	for _, c := range campuses {
		available = append(available, c.Name)
	}
	return nil, NewFlagError(fmt.Sprintf("campus %q not found; available campuses: %s", query, strings.Join(available, ", ")))
}

// ResolveBuilding matches a building in a list by ID, exact name, alias, or substring.
func ResolveBuilding(buildings []Building, query string) (*Building, *CLIError) {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return nil, NewFlagError("building must not be empty")
	}

	// 1. Direct ID match
	if id, err := strconv.Atoi(trimmed); err == nil && id > 0 {
		for i := range buildings {
			if buildings[i].ID == id {
				return &buildings[i], nil
			}
		}
	}

	// 2. Exact name match
	for i := range buildings {
		if buildings[i].Name == trimmed {
			return &buildings[i], nil
		}
	}

	// 3. Alias dictionary resolution
	aliasKeyword, hasAlias := defaultBuildingAliases[trimmed]
	if hasAlias {
		var aliasMatches []*Building
		for i := range buildings {
			if strings.Contains(buildings[i].Name, aliasKeyword) {
				aliasMatches = append(aliasMatches, &buildings[i])
			}
		}
		if len(aliasMatches) == 1 {
			return aliasMatches[0], nil
		}
		if len(aliasMatches) > 1 {
			// e.g. "二教" matches "二教（45楼、46楼）"
			for _, m := range aliasMatches {
				if strings.Contains(m.Name, trimmed) {
					return m, nil
				}
			}
			return aliasMatches[0], nil
		}
	}

	// 4. Clean prefix zeros and clean suffixes
	cleanQuery := strings.TrimPrefix(trimmed, "0")
	var matches []*Building
	for i := range buildings {
		cleanName := strings.TrimPrefix(buildings[i].Name, "0")
		if cleanName == cleanQuery || buildings[i].Name == trimmed {
			return &buildings[i], nil
		}
		if strings.Contains(buildings[i].Name, trimmed) || strings.Contains(buildings[i].Name, cleanQuery) {
			matches = append(matches, &buildings[i])
		}
	}

	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		// Prefer match that starts with or is closely aligned
		for _, m := range matches {
			if strings.HasPrefix(m.Name, trimmed) {
				return m, nil
			}
		}
		var names []string
		for _, m := range matches {
			names = append(names, m.Name)
		}
		return nil, NewFlagError(fmt.Sprintf("building %q is ambiguous (%s); please specify exact building name", query, strings.Join(names, ", ")))
	}

	return nil, NewFlagError(fmt.Sprintf("building %q not found", query))
}

func parseBuildingID(raw json.RawMessage) (int, error) {
	if len(raw) == 0 {
		return 0, errors.New("empty building_id")
	}
	var num int
	if err := json.Unmarshal(raw, &num); err == nil {
		return num, nil
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return strconv.Atoi(str)
	}
	return 0, errors.New("unsupported building_id format")
}

// ValidateDate ensures that the date is in strict yyyy-MM-dd format with valid calendar ranges.
func ValidateDate(date string) (string, *CLIError) {
	trimmed := strings.TrimSpace(date)
	if len(trimmed) != 10 {
		return "", NewFlagError("date must be in yyyy-MM-dd format (e.g. 2026-09-09)")
	}
	parsed, err := time.Parse("2006-01-02", trimmed)
	if err != nil {
		return "", NewFlagError("date must be in yyyy-MM-dd format (e.g. 2026-09-09)")
	}
	formatted := parsed.Format("2006-01-02")
	if formatted != trimmed {
		return "", NewFlagError("date must be in yyyy-MM-dd format (e.g. 2026-09-09)")
	}
	return formatted, nil
}

// TodayInShanghai returns the current date in the Asia/Shanghai time zone.
func TodayInShanghai() string {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	return time.Now().In(loc).Format("2006-01-02")
}

func validateQueryString(value, fieldName string) *CLIError {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return NewFlagError(fmt.Sprintf("%s must not be empty", fieldName))
	}
	if !utf8.ValidString(trimmed) {
		return NewFlagError(fmt.Sprintf("%s must be valid UTF-8", fieldName))
	}
	if utf8.RuneCountInString(trimmed) > 64 {
		return NewFlagError(fmt.Sprintf("%s must not exceed 64 characters", fieldName))
	}
	for _, r := range trimmed {
		if unicode.IsControl(r) {
			return NewFlagError(fmt.Sprintf("%s must not contain control characters", fieldName))
		}
	}
	return nil
}
