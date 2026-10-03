//go:build unix

package main

// Host execution needs per-invocation descendant cancellation. Keep this gate
// aligned with the runtime and MCP process-group supervision support.
const remoteExecutionSupported = true
