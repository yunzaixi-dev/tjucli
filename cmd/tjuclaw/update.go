package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Self-update for a tjuclaw installed by the one-line installer. The release
// manifest (cli/latest.json) names each platform's binary and its SHA-256;
// the manifest is fetched over HTTPS and the binary must come from the same
// host. A copy shipped inside the desktop client is left to the client.

const defaultUpdateURL = "https://tjuclaw-release.zaixi.dev/cli/latest.json"

const maxUpdateBinary = 64 << 20

type updateManifest struct {
	Version     string                        `json:"version"`
	PublishedAt string                        `json:"published_at"`
	Files       map[string]updateManifestFile `json:"files"`
}

type updateManifestFile struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// executablePath is replaceable in tests, which must not overwrite themselves.
var executablePath = func() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(path)
}

func updateURL() string {
	if custom := os.Getenv("TJUCLAW_UPDATE_URL"); custom != "" {
		return custom
	}
	return defaultUpdateURL
}

// trustedUpdateURL accepts HTTPS, and plain HTTP only to this machine (tests).
func trustedUpdateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return nil, errors.New("invalid url")
	}
	switch u.Scheme {
	case "https":
		return u, nil
	case "http":
		if ip := net.ParseIP(u.Hostname()); (ip != nil && ip.IsLoopback()) || u.Hostname() == "localhost" {
			return u, nil
		}
	}
	return nil, errors.New("update urls must use https")
}

// newerVersion reports whether candidate is a later x.y.z than current.
// Anything unparsable is never newer, so a bad manifest cannot downgrade.
func newerVersion(candidate, current string) bool {
	parse := func(v string) ([3]int, bool) {
		var out [3]int
		parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
		if len(parts) != 3 {
			return out, false
		}
		for i, part := range parts {
			n, err := strconv.Atoi(part)
			if err != nil || n < 0 {
				return out, false
			}
			out[i] = n
		}
		return out, true
	}
	c, ok := parse(candidate)
	if !ok {
		return false
	}
	cur, ok := parse(current)
	if !ok {
		// A development build ("dev") updates to any release.
		return current == "dev"
	}
	for i := range c {
		if c[i] != cur[i] {
			return c[i] > cur[i]
		}
	}
	return false
}

// desktopBundled reports whether this tjuclaw ships inside the desktop
// client, which installs it beside its own executable (tjuclaw-client) or in
// a macOS app bundle. That copy is updated with the client.
func desktopBundled(exePath string) bool {
	clean := filepath.Clean(exePath)
	if strings.Contains(clean, ".app"+string(filepath.Separator)+"Contents"+string(filepath.Separator)) {
		return true
	}
	dir := filepath.Dir(clean)
	for _, sibling := range []string{"tjuclaw-client", "tjuclaw-client.exe"} {
		if info, err := os.Stat(filepath.Join(dir, sibling)); err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}

func fetchUpdateManifest(ctx context.Context, client *http.Client, raw string) (*updateManifest, *url.URL, error) {
	source, err := trustedUpdateURL(raw)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.String(), nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	var manifest updateManifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&manifest); err != nil {
		return nil, nil, err
	}
	if !newerVersion(manifest.Version, "0.0.0") {
		return nil, nil, errors.New("invalid manifest version")
	}
	return &manifest, source, nil
}

func replaceExecutable(targetPath string, content []byte) error {
	dir := filepath.Dir(targetPath)
	tempFile, err := os.CreateTemp(dir, ".tjuclaw-update-*")
	if err != nil {
		return err
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)
	if _, err := tempFile.Write(content); err != nil {
		_ = tempFile.Close()
		return err
	}
	if err := tempFile.Chmod(0o755); err != nil {
		_ = tempFile.Close()
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		// A running .exe cannot be overwritten, only renamed aside.
		backup := targetPath + ".old"
		_ = os.Remove(backup)
		if err := os.Rename(targetPath, backup); err != nil {
			return err
		}
		if err := os.Rename(tempPath, targetPath); err != nil {
			_ = os.Rename(backup, targetPath)
			return err
		}
		_ = os.Remove(backup) // still locked while this process runs
		return nil
	}
	return os.Rename(tempPath, targetPath)
}

// checkLatestVersionNotice prints one line when a newer release exists.
// It never updates anything and stays silent on any failure.
func checkLatestVersionNotice(ctx context.Context, errOut io.Writer, currentVersion string) {
	if currentVersion == "dev" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	manifest, _, err := fetchUpdateManifest(ctx, &http.Client{Timeout: 3 * time.Second}, updateURL())
	if err != nil || !newerVersion(manifest.Version, currentVersion) {
		return
	}
	exe, err := executablePath()
	if err == nil && desktopBundled(exe) {
		_, _ = fmt.Fprintf(errOut, "提示：tjuclaw 有新版本 %s（当前 %s），请更新桌面客户端。\n", manifest.Version, currentVersion)
		return
	}
	_, _ = fmt.Fprintf(errOut, "提示：tjuclaw 有新版本 %s（当前 %s），运行 tjuclaw update 更新。\n", manifest.Version, currentVersion)
}

func (r runner) update(ctx context.Context, args []string) int {
	var checkOnly bool
	if _, err := parseFlags(args, func(fs *flag.FlagSet) {
		fs.BoolVar(&checkOnly, "check", false, "")
	}); err != nil {
		return r.respond(nil, "usage_required")
	}
	exePath, err := executablePath()
	if err != nil {
		return r.respond(nil, "update_executable_unknown")
	}
	client := &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	manifest, source, err := fetchUpdateManifest(ctx, client, updateURL())
	if err != nil {
		return r.respond(nil, "update_manifest_unavailable")
	}
	available := newerVersion(manifest.Version, version)
	bundled := desktopBundled(exePath)
	if checkOnly || !available {
		switch {
		case !available:
			_, _ = fmt.Fprintf(r.errOut, "已是最新版本 %s。\n", version)
		case bundled:
			_, _ = fmt.Fprintf(r.errOut, "有新版本 %s（当前 %s），请更新桌面客户端。\n", manifest.Version, version)
		default:
			_, _ = fmt.Fprintf(r.errOut, "有新版本 %s（当前 %s），运行 tjuclaw update 更新。\n", manifest.Version, version)
		}
		return r.respond(map[string]any{
			"current": version, "latest": manifest.Version, "update_available": available, "updated": false,
		}, "")
	}
	if bundled {
		_, _ = io.WriteString(r.errOut, "这个 tjuclaw 由桌面客户端内置，请通过更新桌面客户端来更新它。\n")
		return r.respond(nil, "update_managed_by_desktop")
	}

	file, ok := manifest.Files[runtime.GOOS+"-"+runtime.GOARCH]
	if !ok {
		return r.respond(nil, "update_platform_unsupported")
	}
	download, err := trustedUpdateURL(file.URL)
	if err != nil || download.Host != source.Host || len(file.SHA256) != 64 {
		return r.respond(nil, "update_manifest_unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, download.String(), nil)
	if err != nil {
		return r.respond(nil, "update_download_failed")
	}
	resp, err := client.Do(req)
	if err != nil {
		return r.respond(nil, "update_download_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return r.respond(nil, "update_download_failed")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUpdateBinary+1))
	if err != nil || len(body) > maxUpdateBinary {
		return r.respond(nil, "update_download_failed")
	}
	sum := sha256.Sum256(body)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), file.SHA256) {
		return r.respond(nil, "update_checksum_mismatch")
	}
	_, _ = fmt.Fprintf(r.errOut, "正在更新 tjuclaw %s → %s…\n", version, manifest.Version)
	if err := replaceExecutable(exePath, body); err != nil {
		_, _ = fmt.Fprintf(r.errOut, "无法替换 %s：没有写入权限，或文件正被占用。\n", exePath)
		return r.respond(nil, "update_replace_failed")
	}
	_, _ = fmt.Fprintf(r.errOut, "已更新到 %s。正在运行的 tjuclaw connect 重启后生效。\n", manifest.Version)
	return r.respond(map[string]any{"previous": version, "version": manifest.Version, "updated": true}, "")
}
