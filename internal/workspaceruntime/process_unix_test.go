//go:build unix

package workspaceruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestUnixPromptProcessTreeSupervisionEnabled(t *testing.T) {
	if !promptProcessTreeSupported {
		t.Fatal("Unix invocation process-group supervision unexpectedly disabled")
	}
}

func TestCancellationKillsDescendants(t *testing.T) {
	for _, capability := range []string{"pi.prompt", "claude.prompt", "codex.prompt"} {
		t.Run(capability, func(t *testing.T) {
			e := fakeExecutor(t, capability)
			setMode(t, e, "descendant")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := e.Execute(ctx, capability, json.RawMessage(`{"prompt":"ok"}`))
				done <- err
			}()
			pidPath := filepath.Join(e.Config.Root, ".fake-descendant-pid")
			deadline := time.Now().Add(3 * time.Second)
			var pid int
			for time.Now().Before(deadline) {
				data, _ := os.ReadFile(pidPath)
				pid, _ = strconv.Atoi(string(data))
				if pid > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if pid == 0 {
				cancel()
				t.Fatal("fake descendant did not start")
			}
			defer syscall.Kill(pid, syscall.SIGKILL)
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, ErrCanceled) {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("descendant kept adapter blocked")
			}
			// Linux may retain an orphaned zombie until the container's PID 1
			// reaps it. A zombie is terminated, not a running descendant.
			for i := 0; i < 100; i++ {
				err := syscall.Kill(pid, 0)
				stat, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
				if errors.Is(err, syscall.ESRCH) || strings.Contains(string(stat), ") Z ") {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatal("descendant remains alive after cancellation")
		})
	}
}
