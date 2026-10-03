//go:build !windows && !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package workspaceconfig

// Root confinement, Lstat, and SameFile checks still apply on these platforms.
const safeReadFlags = 0

const readNoFollow = false
