//go:build !unix

package main

// Metadata, configuration and source-only connections remain usable, but a
// connector-wide process supervisor cannot contain each invocation's timeout.
const remoteExecutionSupported = false
