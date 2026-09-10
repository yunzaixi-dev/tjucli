package tjucli

import "fmt"

const (
	DefaultRoomsBaseURL    = "https://selfstudy.twt.edu.cn"
	DefaultRoomsTimeoutSec = 15
	MaxRoomsResponseBytes  = 2 * 1024 * 1024 // 2 MiB
	MinSessionIndex        = 1
	MaxSessionIndex        = 12
)

// Campus represents a university campus.
type Campus struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// Building represents an academic or campus building.
type Building struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	CampusID int    `json:"campus_id"`
}

// RoomStatus represents the real-time availability of a room for a given date and session.
type RoomStatus struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	BuildingID int    `json:"building_id"`
	Free       bool   `json:"free"`
}

// CampusesResult is the response payload for listing campuses.
type CampusesResult struct {
	Campuses []Campus `json:"campuses"`
}

// BuildingsResult is the response payload for listing buildings in a campus.
type BuildingsResult struct {
	CampusID   int        `json:"campus_id"`
	CampusName string     `json:"campus_name,omitempty"`
	Buildings  []Building `json:"buildings"`
}

// RoomQuery contains the parameters for checking room availability.
type RoomQuery struct {
	Campus   string `json:"campus,omitempty"`
	Building string `json:"building"`
	Date     string `json:"date,omitempty"`
	Session  int    `json:"session"`
}

// RoomQueryResult is the response payload for room availability checks.
type RoomQueryResult struct {
	Campus     string       `json:"campus,omitempty"`
	CampusID   int          `json:"campus_id,omitempty"`
	Building   string       `json:"building"`
	BuildingID int          `json:"building_id"`
	Date       string       `json:"date"`
	Session    int          `json:"session"`
	Total      int          `json:"total"`
	FreeCount  int          `json:"free_count"`
	Rooms      []RoomStatus `json:"rooms"`
}

// Validate validates the RoomQuery parameters.
func (q *RoomQuery) Validate() *CLIError {
	if q.Building == "" {
		return NewFlagError("building must not be empty")
	}
	if q.Session < MinSessionIndex || q.Session > MaxSessionIndex {
		return NewFlagError(fmt.Sprintf("session must be between %d and %d", MinSessionIndex, MaxSessionIndex))
	}
	return nil
}
