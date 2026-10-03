//go:build windows

package workspacebridge

// Native Windows fixtures use only temporary files and stdlib Win32 calls.
// They neither invoke a shell nor include user SIDs in test diagnostics.
import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"unsafe"

	"github.com/yunzaixi-dev/tjucli/internal/workspaceconfig"
)

func setSharedOutboxACL(t *testing.T, path string, directory bool) {
	t.Helper()
	text := "D:P(A;;FA;;;WD)" // Protected Everyone full-access DACL.
	if directory {
		text = "D:P(A;OICI;FA;;;WD)"
	}
	sddl, err := syscall.UTF16PtrFromString(text)
	if err != nil {
		t.Fatal("invalid ACL test fixture")
	}
	advapi := syscall.NewLazyDLL("advapi32.dll")
	var descriptor unsafe.Pointer
	ok, _, _ := advapi.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW").Call(
		uintptr(unsafe.Pointer(sddl)), 1, uintptr(unsafe.Pointer(&descriptor)), 0)
	runtime.KeepAlive(sddl)
	if ok == 0 || descriptor == nil {
		t.Fatal("ACL test descriptor construction failed")
	}
	defer syscall.LocalFree(syscall.Handle(uintptr(descriptor)))
	var present, defaulted int32
	var dacl unsafe.Pointer
	ok, _, _ = advapi.NewProc("GetSecurityDescriptorDacl").Call(
		uintptr(descriptor), uintptr(unsafe.Pointer(&present)),
		uintptr(unsafe.Pointer(&dacl)), uintptr(unsafe.Pointer(&defaulted)))
	if ok == 0 || present == 0 || dacl == nil {
		t.Fatal("ACL test DACL construction failed")
	}
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal("invalid ACL test path")
	}
	const writeDAC = 0x00040000
	flags := uint32(syscall.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		flags |= syscall.FILE_FLAG_BACKUP_SEMANTICS
	}
	// SetSecurityInfo reads the current descriptor too, so READ_CONTROL is needed.
	const readControl = 0x00020000
	handle, err := syscall.CreateFile(name, writeDAC|readControl,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, flags, 0)
	if err != nil {
		t.Fatal("ACL test handle unavailable")
	}
	defer syscall.CloseHandle(handle)
	const (
		seFileObject     = 1
		daclSecurityInfo = 0x4
		protectDACLInfo  = 0x80000000
	)
	status, _, _ := advapi.NewProc("SetSecurityInfo").Call(
		uintptr(handle), seFileObject, daclSecurityInfo|protectDACLInfo, 0, 0, uintptr(dacl), 0)
	if status != 0 {
		t.Fatalf("ACL test application failed: status %d", status)
	}
}

func TestWindowsOutboxRejectsSharedDirectoriesWithoutRepair(t *testing.T) {
	for _, level := range []string{"config", "parent", "connection"} {
		t.Run(level, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			client, err := New(server.URL, "test-only-token")
			if err != nil {
				t.Fatal(err)
			}
			dir := privateConfigDir(t)
			outbox, err := client.CompletionOutbox(dir)
			if err != nil {
				t.Fatal(err)
			}
			item := pendingCompletion{WorkspaceID: "target", InvocationID: "job", Result: json.RawMessage(`{}`)}
			if err := outbox.save(item); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(outbox.dir, "job.json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal("private result unavailable")
			}
			unsafeDir := dir
			if level == "parent" {
				unsafeDir = filepath.Dir(outbox.dir)
			} else if level == "connection" {
				unsafeDir = outbox.dir
			}
			setSharedOutboxACL(t, unsafeDir, true)
			if _, err := client.CompletionOutbox(dir); err == nil {
				t.Fatal("constructor accepted shared outbox storage")
			}
			if outbox.save(pendingCompletion{WorkspaceID: "target", InvocationID: "other", Result: json.RawMessage(`{}`)}) == nil {
				t.Fatal("save accepted shared outbox storage")
			}
			if outbox.Flush(context.Background(), client, "target") == nil || requests.Load() != 0 {
				t.Fatal("Flush accepted shared storage or contacted server")
			}
			if workspaceconfig.CheckPrivateDir(unsafeDir) == nil {
				t.Fatal("outbox silently repaired shared directory")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("outbox erased or changed undelivered result")
			}
		})
	}
}

func TestWindowsOutboxRejectsSharedResultWithoutRepairOrDelivery(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := New(server.URL, "test-only-token")
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := client.CompletionOutbox(privateConfigDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := outbox.save(pendingCompletion{WorkspaceID: "target", InvocationID: "job", Result: json.RawMessage(`{"output":"fixture"}`)}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(outbox.dir, "job.json")
	if workspaceconfig.CheckPrivateFile(path) != nil {
		t.Fatal("captured result lacks a private ACL")
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("private result unavailable")
	}
	setSharedOutboxACL(t, path, false)
	if outbox.Flush(context.Background(), client, "target") == nil || requests.Load() != 0 {
		t.Fatal("Flush read shared result or contacted server")
	}
	if workspaceconfig.CheckPrivateFile(path) == nil {
		t.Fatal("Flush repaired shared result ACL")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("Flush changed or removed undelivered result")
	}
	// Only an explicit local-owner repair permits subsequent recovery.
	if err := workspaceconfig.EnsurePrivateFile(path); err != nil {
		t.Fatal(err)
	}
	if outbox.Flush(context.Background(), client, "target") != nil || requests.Load() != 1 {
		t.Fatal("explicitly repaired result did not recover")
	}
}
