package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/app"
)

func TestMigratePathsPreviewThenApplyOnEmptyRepository(t *testing.T) {
	dir, _ := setupCLI(t)
	var preview app.MigrateResult
	if err := json.Unmarshal([]byte(succeed(t, invoke(dir, nil, "migrate", "paths", "--json"))), &preview); err != nil {
		t.Fatal(err)
	}
	if !preview.DryRun || preview.Prune || preview.Migrated != 0 || len(preview.Items) != 0 || preview.Head == "" {
		t.Fatalf("preview = %#v", preview)
	}
	if _, err := os.Stat(filepath.Join(dir, "migration", "plan.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preview wrote a migration plan: %v", err)
	}

	var applied app.MigrateResult
	if err := json.Unmarshal([]byte(succeed(t, invoke(dir, nil, "migrate", "paths", "--apply", "--json"))), &applied); err != nil {
		t.Fatal(err)
	}
	if applied.DryRun || applied.Migrated != 0 || applied.Head == preview.Head {
		t.Fatalf("apply = %#v", applied)
	}
	// With no path changes the CLI must not emit an empty rewrite mapping.
	if _, err := os.Stat(filepath.Join(dir, "migration", "url-mapping.csv")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("apply wrote an empty mapping: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "migration", "url-mapping.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("apply wrote an empty mapping: %v", err)
	}
}
