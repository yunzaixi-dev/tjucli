//go:build !unix && !windows

package workspaceruntime

import "context"

// Fail closed rather than claiming cross-process conversation serialization.
func lockSession(context.Context, string) (func(), error) {
	return nil, ErrRuntimeUnavailable
}
