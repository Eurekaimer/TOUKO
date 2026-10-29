package webview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperationLogRedactsCredentials(t *testing.T) {
	f := newFixture(t)
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "r2-secret-sentinel")
	if err := f.server.recordOperationFailure("/api/commit", 502, "api_error", "sentinel-private-token r2-secret-sentinel failed"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.service.Dir, "operation-errors.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "sentinel-private-token") || strings.Contains(string(data), "r2-secret-sentinel") || !strings.Contains(string(data), "[redacted]") {
		t.Fatalf("diagnostic leaked credentials: %s", data)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("log permissions: %v %v", info, err)
	}
}

func TestOperationFailureReportsLogWriteFailure(t *testing.T) {
	f := newFixture(t)
	f.setup()
	if err := os.Mkdir(filepath.Join(f.service.Dir, "operation-errors.jsonl"), 0700); err != nil {
		t.Fatal(err)
	}
	response := f.request("POST", "/api/commit", map[string]string{"message": ""}, 400)
	if response.Header().Get("X-Touko-Log-Error") != "1" || !strings.Contains(response.Body.String(), "commit message must not be empty") {
		t.Fatalf("missing original error or log warning: %s", response.Body.String())
	}
}
