//go:build windows

package workspaceconfig

import "syscall"

// Open the final reparse point itself rather than following it. File type and
// native attribute checks reject such handles before ACL changes or reads.
// As on Unix, the opened regular inode can safely differ from Lstat when an
// atomic Save replaced the config in between.
const safeReadFlags = syscall.FILE_FLAG_OPEN_REPARSE_POINT

const readNoFollow = true
