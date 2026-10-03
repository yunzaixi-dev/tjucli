//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package workspacemcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/yunzaixi-dev/tjucli/internal/workspaceconfig"
)

// These fake processes exercise inherited pipes and descendant lifetime.
// They do not invoke a shell, model, installed MCP server or network service.
func TestMCPProcessTreeHelper(t *testing.T) {
	mode := os.Getenv("MCP_TREE_MODE")
	marker := os.Getenv("MCP_TREE_MARKER")
	if mode == "" {
		return
	}
	if mode == "child" {
		for n := 0; ; n++ {
			if err := os.WriteFile(marker, []byte(strconv.Itoa(n)), 0600); err != nil {
				os.Exit(20)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		os.Exit(21)
	}
	child := exec.Command(executable, "-test.run=^TestMCPProcessTreeHelper$")
	child.Env = append(os.Environ(), "MCP_TREE_MODE=child")
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if child.Start() != nil {
		os.Exit(22)
	}
	if os.WriteFile(marker+".pid", []byte(fmt.Sprint(child.Process.Pid)), 0600) != nil {
		os.Exit(23)
	}
	if mode == "exit" {
		os.Exit(0) // descendant holds stdout open after the direct server exits
	}
	time.Sleep(time.Minute)
	os.Exit(0)
}

func TestCancellationStopsDescendantsWithInheritedPipes(t *testing.T) {
	for _, mode := range []string{"hang", "exit"} {
		t.Run(mode, func(t *testing.T) {
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "heartbeat")
			t.Setenv("MCP_TREE_TEST_MODE", mode)
			t.Setenv("MCP_TREE_TEST_MARKER", marker)
			server := workspaceconfig.MCPServer{
				Command: executable,
				Args:    []string{"-test.run=^TestMCPProcessTreeHelper$"},
				Env: map[string]string{
					"MCP_TREE_MODE": "MCP_TREE_TEST_MODE", "MCP_TREE_MARKER": "MCP_TREE_TEST_MARKER",
				},
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := Call(ctx, t.TempDir(), server, "echo", json.RawMessage(`{}`))
				done <- err
			}()
			deadline := time.Now().Add(5 * time.Second)
			for {
				if data, err := os.ReadFile(marker + ".pid"); err == nil {
					pid, err := strconv.Atoi(string(data))
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						// Failsafe for a supervision regression.
						if process, err := os.FindProcess(pid); err == nil {
							_ = process.Kill()
						}
					})
					if _, err := os.Stat(marker); err == nil {
						break
					}
				}
				if time.Now().After(deadline) {
					t.Fatal("fake descendant did not start")
				}
				time.Sleep(10 * time.Millisecond)
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("inherited stdout prevented cancellation")
			}
			time.Sleep(50 * time.Millisecond)
			before, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond)
			after, err := os.ReadFile(marker)
			if err != nil || string(before) != string(after) {
				t.Fatal("descendant continued executing after Call returned")
			}
		})
	}
}
