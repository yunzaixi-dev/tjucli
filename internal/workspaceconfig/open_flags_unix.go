//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package workspaceconfig

import "syscall"

// Nonblocking protects against replacement with a FIFO between Lstat and Open.
const safeReadFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK

const readNoFollow = true
