package workspaceruntime

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"sync"
	"time"
)

// cappedWriter stops the whole process on overflow without retaining more
// than the limit. Stderr is bounded but never returned to a caller.
type cappedWriter struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	limit    int
	exceeded bool
	cancel   context.CancelFunc
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.exceeded || len(p) > w.limit-w.buf.Len() {
		w.exceeded = true
		w.cancel()
		return 0, ErrOutputLimit
	}
	return w.buf.Write(p)
}

func (w *cappedWriter) overflow() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.exceeded
}

func (e Executor) command(ctx context.Context, state runState, binary string, args []string) (*exec.Cmd, error) {
	path, err := exec.LookPath(binary)
	if err != nil {
		return nil, ErrRuntimeUnavailable
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = e.Config.Root
	cmd.Env = state.env
	cmd.WaitDelay = time.Second
	configureProcess(cmd)
	return cmd, nil
}

func (e Executor) printProcess(ctx context.Context, state runState, binary string, args []string, input string) ([]byte, error) {
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd, err := e.command(childCtx, state, binary, args)
	if err != nil {
		return nil, err
	}
	stdout := &cappedWriter{limit: maxOutput, cancel: cancel}
	stderr := &cappedWriter{limit: maxStderr, cancel: cancel}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Stdin = bytes.NewBufferString(input)
	err = cmd.Run()
	if stdout.overflow() || stderr.overflow() {
		return nil, ErrOutputLimit
	}
	if ctx.Err() != nil {
		return nil, ErrCanceled
	}
	if err != nil {
		return nil, ErrRuntimeFailed
	}
	return stdout.buf.Bytes(), nil
}

// A LimitedReader alone could accept a valid prefix and ignore a flood. A
// budgetReader treats an extra byte as failure and cancels the subprocess.
type budgetReader struct {
	reader io.Reader
	left   int64
	cancel context.CancelFunc
}

func (r *budgetReader) Read(p []byte) (int, error) {
	if r.left == 0 {
		var extra [1]byte
		n, err := r.reader.Read(extra[:])
		if n != 0 {
			r.cancel()
			return 0, ErrOutputLimit
		}
		return 0, err
	}
	if int64(len(p)) > r.left {
		p = p[:r.left]
	}
	n, err := r.reader.Read(p)
	r.left -= int64(n)
	return n, err
}
