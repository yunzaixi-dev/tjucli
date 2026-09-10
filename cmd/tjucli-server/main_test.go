package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func createTestGrantsFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "grants.json")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatalf("failed to write grants file: %v", err)
	}
	return p
}

func getFreePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to get free port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func TestRun_MissingGrantsFile(t *testing.T) {
	err := run([]string{})
	if err == nil || !strings.Contains(err.Error(), "grants file must be specified") {
		t.Fatalf("expected missing grants file error, got %v", err)
	}
}

func TestRun_InvalidGrantsFile(t *testing.T) {
	path := createTestGrantsFile(t, `{"bad":true}`, 0600)
	err := run([]string{"-grants-file", path})
	if err == nil || !strings.Contains(err.Error(), "invalid grants file") {
		t.Fatalf("expected invalid grants file error, got %v", err)
	}
}

func TestRun_GracefulShutdown(t *testing.T) {
	token := "run-test-token"
	tokenHex := hashToken(token)
	expiry := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	grantsJSON := fmt.Sprintf(`{"grants":[{"token_sha256":"%s","run_id":"0123456789abcdef0123456789abcdef","expires_at":"%s","scopes":["course:read"]}]}`, tokenHex, expiry)
	grantsPath := createTestGrantsFile(t, grantsJSON, 0600)

	addr := getFreePort(t)

	done := make(chan error, 1)
	go func() {
		done <- run([]string{"-addr", addr, "-grants-file", grantsPath})
	}()

	// Wait for server to start listening
	var conn net.Conn
	var dialErr error
	for i := 0; i < 20; i++ {
		conn, dialErr = net.Dial("tcp", addr)
		if dialErr == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if dialErr != nil {
		t.Fatalf("server failed to start listening on %s: %v", addr, dialErr)
	}

	// Trigger SIGTERM to current process to test graceful shutdown handling
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected graceful shutdown without error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for server shutdown")
	}
}
