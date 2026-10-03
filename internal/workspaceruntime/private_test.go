package workspaceruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/yunzaixi-dev/tjucli/internal/workspaceconfig"
)

func TestPrivateRuntimeWritesUsePlatformPrivacyAndAtomicReplacement(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runtime")
	if privateDir(dir) != nil {
		t.Fatal("private directory")
	}
	if workspaceconfig.CheckPrivateDir(dir) != nil {
		t.Fatal("runtime directory lacks platform-private permissions")
	}
	path := filepath.Join(dir, "secret.json")
	for _, value := range []string{"first synthetic secret", "replacement synthetic secret"} {
		if privateBytes(path, []byte(value)) != nil || workspaceconfig.CheckPrivateFile(path) != nil {
			t.Fatal("generated file lacks platform-private permissions")
		}
		data, err := readPrivateFile(path, maxArguments)
		if err != nil || string(data) != value {
			t.Fatal("private atomic replacement/read", err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("temporary secret files retained")
	}
	if _, err := readPrivateFile(path, 2); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal("private read ignored byte bound")
	}
}

func TestRuntimePrivacyRejectsFileAndAncestorSymlinks(t *testing.T) {
	private := filepath.Join(t.TempDir(), "private")
	outside := filepath.Join(t.TempDir(), "outside")
	if privateDir(private) != nil || privateDir(outside) != nil {
		t.Fatal("private fixtures")
	}
	target := filepath.Join(outside, "source")
	if privateBytes(target, []byte("unchanged")) != nil {
		t.Fatal("private target fixture")
	}
	fileLink := filepath.Join(private, "file-link")
	if err := os.Symlink(target, fileLink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	dirLink := filepath.Join(private, "dir-link")
	if os.Symlink(outside, dirLink) != nil {
		t.Fatal("directory link fixture")
	}
	for _, path := range []string{fileLink, filepath.Join(dirLink, "source")} {
		if !errors.Is(privateBytes(path, []byte("not written")), ErrInvalidConfig) {
			t.Fatal("write followed symlink")
		}
		if _, err := readPrivateFile(path, maxArguments); !errors.Is(err, ErrInvalidConfig) {
			t.Fatal("read followed symlink")
		}
	}
	actual, _ := os.ReadFile(target)
	if string(actual) != "unchanged" {
		t.Fatal("foreign symlink target changed")
	}
}

func TestRuntimeHostCredentialPrivacyCheckDoesNotRepairSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission fixture; Windows ACL checks are shared-helper tests")
	}
	dir := t.TempDir()
	if os.Chmod(dir, 0755) != nil {
		t.Fatal("permissive source directory fixture")
	}
	path := filepath.Join(dir, "credential")
	if os.WriteFile(path, []byte("synthetic private credential"), 0600) != nil {
		t.Fatal("source credential fixture")
	}
	if _, err := readPrivateFile(path, maxArguments); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal("credential in nonprivate parent accepted")
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatal("source parent permissions changed")
	}
}

func TestPendingApprovalAggregateLeavesEnvelopeBudgetAndNeverTruncates(t *testing.T) {
	e := fakeExecutor(t, "pi.prompt")
	sessionID, _ := sessionPrompt(t, e, "pi", "", "fixture")
	dir, err := e.ApprovalDirectory()
	if err != nil {
		t.Fatal(err)
	}
	request := ApprovalRequest{
		Runtime: "pi", SessionID: sessionID, Kind: "test",
		Input:     json.RawMessage(append(append([]byte{'"'}, bytes.Repeat([]byte("x"), maxPrompt-2)...), '"')),
		ExpiresAt: time.Now().Add(approvalWait),
	}
	size, count := 2, 0
	for {
		request.ID, _ = randomID()
		encoded, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		added := len(encoded)
		if count != 0 {
			added++
		}
		if size+added > maxPendingApprovalBytes {
			// The full queue still fits native's envelope after a successful
			// response; one more entry must fail rather than hide entries.
			items, err := e.PendingApprovals(context.Background())
			if err != nil || len(items) != count {
				t.Fatal("bounded approval aggregate rejected", err)
			}
			public, err := json.Marshal(items)
			if err != nil || len(public) > maxPendingApprovalBytes || len(public)+(4<<10) > 1<<20 {
				t.Fatal("native envelope budget exceeded")
			}
		}
		requestDir := filepath.Join(dir, request.ID)
		if privateDir(requestDir) != nil ||
			atomicPrivateJSON(filepath.Join(requestDir, "request.json"), request) != nil {
			t.Fatal("private approval fixture")
		}
		if size+added > maxPendingApprovalBytes {
			items, err := e.PendingApprovals(context.Background())
			if !errors.Is(err, ErrOutputLimit) || items != nil {
				t.Fatal("oversize approval aggregate silently truncated or exposed", err)
			}
			break
		}
		size += added
		count++
	}
}

func TestPrivateSessionLockSerializesAndHonorsCanceledContext(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("platform-specific lock support")
	}
	dir := filepath.Join(t.TempDir(), "private")
	if privateDir(dir) != nil {
		t.Fatal("private lock fixture")
	}
	path := filepath.Join(dir, ".lock")
	unlock, err := lockSession(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if workspaceconfig.CheckPrivateFile(path) != nil {
		unlock()
		t.Fatal("lock lacks platform-private permissions")
	}
	if second, err := lockSession(context.Background(), path); !errors.Is(err, ErrSessionBusy) {
		if second != nil {
			second()
		}
		unlock()
		t.Fatal("concurrent lock accepted", err)
	}
	unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if next, err := lockSession(ctx, path); !errors.Is(err, ErrCanceled) {
		if next != nil {
			next()
		}
		t.Fatal("canceled lock acquired", err)
	}
	next, err := lockSession(context.Background(), path)
	if err != nil {
		t.Fatal("released lock not reusable", err)
	}
	next()
}
