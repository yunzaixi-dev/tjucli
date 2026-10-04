package workspacebridge

import (
	"context"
	"encoding/base64"
	"errors"
)

var errInvalidID = errors.New("workspace_id_invalid")

// RelayEvent is what the browser asked of one of this computer's terminals:
// open (with size and directory), input, resize or close.
type RelayEvent struct {
	Type     string `json:"type"`
	Terminal string `json:"terminal"`
	Data     string `json:"data,omitempty"` // base64 keystrokes
	Cols     int    `json:"cols,omitempty"`
	Rows     int    `json:"rows,omitempty"`
	Cwd      string `json:"cwd,omitempty"`
}

// RelayPoll waits up to the server's limit for terminal events.
func (c *Client) RelayPoll(ctx context.Context, workspaceID string) ([]RelayEvent, error) {
	if !validID(workspaceID) {
		return nil, errInvalidID
	}
	var out struct {
		Events []RelayEvent `json:"events"`
	}
	if err := c.request(ctx, "POST", "/workspaces/"+workspaceID+"/relay", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return out.Events, nil
}

// TerminalOutput sends what the shell wrote, and optionally that the
// terminal is now open, or closed with its exit code or a failure code.
func (c *Client) TerminalOutput(ctx context.Context, workspaceID, terminalID string, data []byte, state string, exitCode *int, failure string) error {
	if !validID(workspaceID) || !validID(terminalID) {
		return errInvalidID
	}
	body := map[string]any{"data": base64.StdEncoding.EncodeToString(data)}
	if state != "" {
		body["state"] = state
	}
	if exitCode != nil {
		body["exit_code"] = *exitCode
	}
	if failure != "" {
		body["error"] = failure
	}
	return c.request(ctx, "POST", "/workspaces/"+workspaceID+"/terminals/"+terminalID+"/output", body, nil)
}
