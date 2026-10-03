package workspacebridge

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Execute must re-read local permissions immediately before starting a process.
type Execute func(context.Context, string, json.RawMessage) (json.RawMessage, error)

// Step claims at most one invocation. Failed completion is not re-executed:
// the server expires the running lease instead of replaying host operations.
func (c *Client) Step(ctx context.Context, id string, execute Execute) (bool, error) {
	return c.step(ctx, id, execute, nil)
}

// StepDurable records a finished invocation before attempting delivery. If
// delivery fails, the connector may flush this result after restart without
// claiming or executing the command a second time.
func (c *Client) StepDurable(ctx context.Context, id string, execute Execute, outbox *CompletionOutbox) (bool, error) {
	if outbox == nil {
		return false, errors.New("workspace_outbox_unavailable")
	}
	return c.step(ctx, id, execute, outbox)
}

func (c *Client) step(ctx context.Context, id string, execute Execute, outbox *CompletionOutbox) (bool, error) {
	if execute == nil {
		return false, errors.New("workspace_executor_unavailable")
	}
	// Refuse unsafe/full storage and unresolved completions BEFORE claiming or
	// executing another command. This also protects direct StepDurable callers,
	// not only the daemon's initial reconnect recovery.
	if outbox != nil {
		if err := outbox.Flush(ctx, c, id); err != nil {
			return false, err
		}
	}
	invocation, err := c.Poll(ctx, id)
	if err != nil || invocation == nil {
		return false, err
	}
	if invocation.TargetWorkspaceID != id || invocation.Status != "running" || !validID(invocation.ID) {
		return false, errors.New("workspace_invocation_invalid")
	}
	timeout := 30 * time.Minute
	if invocation.TimeoutSeconds != 0 {
		if invocation.TimeoutSeconds < 1 || invocation.TimeoutSeconds > 86400 {
			return true, errors.New("workspace_invocation_invalid")
		}
		timeout = time.Duration(invocation.TimeoutSeconds) * time.Second
	}
	deadline := time.Now().Add(timeout)
	if invocation.DeadlineAt != nil && invocation.DeadlineAt.Before(deadline) {
		deadline = *invocation.DeadlineAt
	}
	runCtx, cancel := context.WithDeadline(ctx, deadline)
	var result json.RawMessage
	var executionErr error
	if runCtx.Err() != nil {
		executionErr = runCtx.Err()
	} else {
		result, executionErr = execute(runCtx, invocation.Capability, invocation.Arguments)
	}
	cancel()
	if executionErr == nil && (len(result) > maxResultBytes || !json.Valid(result)) {
		executionErr = errors.New("workspace_result_invalid")
	}
	if outbox != nil {
		item := pendingCompletion{WorkspaceID: id, InvocationID: invocation.ID, Result: result, Failed: executionErr != nil}
		if err := outbox.save(item); err != nil {
			return true, err
		}
		return true, outbox.Flush(ctx, c, id)
	}
	if err := c.Complete(ctx, id, invocation.ID, result, executionErr); err != nil {
		return true, err
	}
	return true, nil
}
