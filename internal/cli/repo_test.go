package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/Eurekaimer/TOUKO/internal/localgit"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

func TestSyncReadsNewRemoteInOneInvocation(t *testing.T) {
	ctx := context.Background()
	writer, remote := setupCLI(t)
	succeed(t, invoke(writer, picture(t), "add", "-", "--title", "Before fetch"))
	succeed(t, invoke(writer, nil, "commit", "-m", "first asset"))

	dir := filepath.Join(t.TempDir(), "reader")
	s, err := app.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Select(ctx, repository.Location{Backend: "github", Owner: "fixture", Name: "gallery", Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	// The GitHub workspace already has a clone. Its local fixture origin lets
	// the real Clone/Update path run without network access or fake Git output.
	if err := localgit.Clone(ctx, remote, repository.CloneDir(dir), "main", ""); err != nil {
		t.Fatal(err)
	}
	clone, err := localgit.Open(ctx, repository.CloneDir(dir), "main")
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.Sync(ctx, clone)
	if err != nil || before.AssetCount != 1 {
		t.Fatalf("initial snapshot=%+v err=%v", before, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	succeed(t, invoke(writer, picture(t), "add", "-", "--title", "After fetch"))
	succeed(t, invoke(writer, nil, "commit", "-m", "second asset"))
	origin, err := localgit.Open(ctx, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	wantHead, err := origin.Head(ctx)
	if err != nil || wantHead == before.Head {
		t.Fatalf("origin did not advance: %s %v", wantHead, err)
	}
	// Avoid using the developer's keyring; no GitHub HTTP request is needed.
	t.Setenv("GITHUB_TOKEN", "fixture-token")
	var got app.SyncResult
	if err := json.Unmarshal([]byte(succeed(t, invoke(dir, nil, "sync", "--json"))), &got); err != nil {
		t.Fatal(err)
	}
	if got.Head != wantHead || got.AssetCount != 2 {
		t.Fatalf("single sync read stale clone: got=%+v want head=%s assets=2", got, wantHead)
	}
	views := assetList(t, invoke(dir, nil, "ls", "--json"))
	if len(views) != 2 || views[0].Asset.Title == views[1].Asset.Title {
		t.Fatalf("fresh committed metadata missing: %+v", views)
	}
}
