//go:build !unix

package workspaceruntime

import "os/exec"

// CommandContext alone kills only the immediate process. Execute rejects
// prompts until a per-invocation process-tree supervisor is implemented.
// A connector-wide Job does not make individual invocation deadlines safe.
const promptProcessTreeSupported = false

func configureProcess(cmd *exec.Cmd) {}
