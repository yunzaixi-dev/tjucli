package account

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCredentialsAreOwnerOnlyAndTheEnvironmentWins(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	t.Setenv("TJUCLAW_TOKEN", "")
	t.Setenv("TJUCLAW_API_URL", "")
	if _, err := Load(dir); !errors.Is(err, ErrSignedOut) {
		t.Fatalf("no credentials: %v", err)
	}
	if err := Save(dir, Credentials{API: DefaultAPI, Token: "tjc_saved", Email: "a@tju.edu.cn"}); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(filepath.Join(dir, "auth.json")); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("auth.json mode %v", info.Mode().Perm())
	}
	if c, err := Load(dir); err != nil || c.Token != "tjc_saved" {
		t.Fatalf("load: %+v %v", c, err)
	}
	t.Setenv("TJUCLAW_TOKEN", "tjc_env")
	if c, _ := Load(dir); c.Token != "tjc_env" || c.API != DefaultAPI {
		t.Fatalf("environment token: %+v", c)
	}
	t.Setenv("TJUCLAW_API_URL", "http://example.com/api")
	if _, err := Load(dir); err == nil {
		t.Fatal("plain http API accepted")
	}
	t.Setenv("TJUCLAW_API_URL", "http://127.0.0.1:8080")
	if c, err := Load(dir); err != nil || c.API != "http://127.0.0.1:8080" {
		t.Fatalf("loopback API: %+v %v", c, err)
	}
	if err := Remove(dir); err != nil || Remove(dir) != nil {
		t.Fatal("remove")
	}
}
