package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// releaseServer serves a manifest naming this platform's binary.
func releaseServer(t *testing.T, latest string, binary []byte, sum string) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cli/latest.json":
			_ = json.NewEncoder(w).Encode(map[string]any{"version": latest, "files": map[string]any{
				runtime.GOOS + "-" + runtime.GOARCH: map[string]any{"url": server.URL + "/cli/v" + latest + "/tjuclaw", "sha256": sum},
			}})
		case "/cli/v" + latest + "/tjuclaw":
			_, _ = w.Write(binary)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("TJUCLAW_UPDATE_URL", server.URL+"/cli/latest.json")
	return server
}

// installed puts a fake tjuclaw in a temporary directory and points the
// updater at it, so no test replaces the test binary itself.
func installed(t *testing.T, siblings ...string) string {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "tjuclaw")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range siblings {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("client"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	previous := executablePath
	executablePath = func() (string, error) { return exe, nil }
	t.Cleanup(func() { executablePath = previous })
	return exe
}

func withVersion(t *testing.T, v string) {
	previous := version
	version = v
	t.Cleanup(func() { version = previous })
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestUpdateDownloadsVerifiesAndReplaces(t *testing.T) {
	withVersion(t, "0.1.1")
	exe := installed(t)
	binary := []byte("new tjuclaw 0.1.2")
	releaseServer(t, "0.1.2", binary, sha(binary))
	code, body := runCommand(t, t.TempDir(), "", "update")
	if code != 0 || !strings.Contains(body, `"updated":true`) || !strings.Contains(body, `"version":"0.1.2"`) {
		t.Fatalf("update: %d %s", code, body)
	}
	if got, _ := os.ReadFile(exe); !bytes.Equal(got, binary) {
		t.Fatalf("binary not replaced: %q", got)
	}
	if info, _ := os.Stat(exe); runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		t.Fatal("replacement is not executable")
	}
}

func TestUpdateRefusesAChecksumMismatch(t *testing.T) {
	withVersion(t, "0.1.1")
	exe := installed(t)
	releaseServer(t, "0.1.2", []byte("tampered"), sha([]byte("genuine")))
	code, body := runCommand(t, t.TempDir(), "", "update")
	if code == 0 || !strings.Contains(body, "update_checksum_mismatch") {
		t.Fatalf("mismatch accepted: %d %s", code, body)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old" {
		t.Fatalf("binary changed after a mismatch: %q", got)
	}
}

func TestUpdateNeverDowngrades(t *testing.T) {
	withVersion(t, "0.1.1")
	exe := installed(t)
	binary := []byte("older")
	releaseServer(t, "0.0.30", binary, sha(binary))
	code, body := runCommand(t, t.TempDir(), "", "update")
	if code != 0 || !strings.Contains(body, `"update_available":false`) {
		t.Fatalf("update: %d %s", code, body)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old" {
		t.Fatal("downgraded")
	}
}

func TestUpdateLeavesTheDesktopCopyToTheClient(t *testing.T) {
	withVersion(t, "0.1.1")
	exe := installed(t, "tjuclaw-client")
	binary := []byte("new")
	releaseServer(t, "0.1.2", binary, sha(binary))
	code, body := runCommand(t, t.TempDir(), "", "update")
	if code == 0 || !strings.Contains(body, "update_managed_by_desktop") {
		t.Fatalf("desktop copy updated: %d %s", code, body)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old" {
		t.Fatal("desktop copy replaced")
	}
	// Checking is still allowed, and says where the update comes from.
	if code, body := runCommand(t, t.TempDir(), "", "update", "--check"); code != 0 || !strings.Contains(body, `"update_available":true`) {
		t.Fatalf("check: %d %s", code, body)
	}
}

func TestUpdateOnlyTrustsHTTPSAndTheManifestHost(t *testing.T) {
	withVersion(t, "0.1.1")
	installed(t)
	t.Setenv("TJUCLAW_UPDATE_URL", "http://releases.example/cli/latest.json")
	if code, body := runCommand(t, t.TempDir(), "", "update"); code == 0 || !strings.Contains(body, "update_manifest_unavailable") {
		t.Fatalf("plain http manifest used: %s", body)
	}
	// A manifest pointing the binary at another host is refused.
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("x")) }))
	defer elsewhere.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"version": "0.1.2", "files": map[string]any{
			runtime.GOOS + "-" + runtime.GOARCH: map[string]any{"url": strings.Replace(elsewhere.URL, "127.0.0.1", "localhost", 1) + "/x", "sha256": sha([]byte("x"))},
		}})
	}))
	defer server.Close()
	t.Setenv("TJUCLAW_UPDATE_URL", server.URL+"/cli/latest.json")
	if code, body := runCommand(t, t.TempDir(), "", "update"); code == 0 || !strings.Contains(body, "update_manifest_unavailable") {
		t.Fatalf("cross-host binary accepted: %s", body)
	}
}

func TestNewerVersion(t *testing.T) {
	for _, c := range []struct {
		candidate, current string
		want               bool
	}{
		{"0.1.1", "0.0.30", true}, {"0.1.10", "0.1.9", true}, {"0.1.1", "0.1.1", false},
		{"0.0.30", "0.1.1", false}, {"0.1.1", "dev", true}, {"garbage", "0.1.1", false}, {"0.1", "0.0.1", false},
	} {
		if got := newerVersion(c.candidate, c.current); got != c.want {
			t.Errorf("newerVersion(%q, %q) = %v", c.candidate, c.current, got)
		}
	}
}

func TestVersionNoticeOnlyForNewerReleases(t *testing.T) {
	installed(t)
	for _, c := range []struct {
		latest string
		want   bool
	}{{"0.1.2", true}, {"0.1.1", false}, {"0.0.30", false}} {
		releaseServer(t, c.latest, []byte("x"), sha([]byte("x")))
		var errOut bytes.Buffer
		checkLatestVersionNotice(context.Background(), &errOut, "0.1.1")
		if got := strings.Contains(errOut.String(), "tjuclaw update"); got != c.want {
			t.Errorf("latest %s: notice %q", c.latest, errOut.String())
		}
	}
}
