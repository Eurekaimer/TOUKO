package app

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/repository"
)

type countMetadataGit struct {
	repository.Git
	mu     sync.Mutex
	target repository.BlobID
	reads  int
}

func (g *countMetadataGit) OpenBlob(ctx context.Context, id repository.BlobID) (io.ReadCloser, error) {
	if id == g.target {
		g.mu.Lock()
		g.reads++
		g.mu.Unlock()
	}
	return g.Git.OpenBlob(ctx, id)
}
func TestCommitReusesBaseButRecoveryReadsBothSnapshots(t *testing.T) {
	ctx, git, s := realRepository(t)
	old := stageImage(t, ctx, s)
	base := mustCommit(t, ctx, s, git)
	snap, err := repository.ReadSnapshot(ctx, git, base.Head)
	if err != nil {
		t.Fatal(err)
	}
	var sha repository.BlobID
	for _, entry := range snap.Entries {
		if entry.Path == repository.AssetPath(old.ID) {
			sha = repository.BlobID(entry.SHA)
		}
	}
	if sha == "" {
		t.Fatal("base asset metadata missing")
	}
	if _, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(batchPNG(t, 77)), Name: "other.png"}}, AddOptions{Namespace: "blog"}); err != nil {
		t.Fatal(err)
	}
	fault := &publicationFault{Git: git, fail: "ref"}
	counted := &countMetadataGit{Git: fault, target: sha}
	if _, err = s.Commit(ctx, counted, "new image"); err == nil {
		t.Fatal("expected ref failure")
	}
	if counted.reads != 1 {
		t.Fatalf("normal commit read unchanged metadata %d times", counted.reads)
	}
	recovery := &countMetadataGit{Git: git, target: sha}
	if _, err = s.Commit(ctx, recovery, "retry"); err != nil {
		t.Fatal(err)
	}
	if recovery.reads != 2 {
		t.Fatalf("recovery read unchanged metadata %d times, want candidate and base", recovery.reads)
	}
}
