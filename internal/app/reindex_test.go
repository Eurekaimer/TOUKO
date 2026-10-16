package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/index"
	"github.com/Eurekaimer/TOUKO/internal/localgit"
	"github.com/Eurekaimer/TOUKO/internal/repository"
	"github.com/Eurekaimer/TOUKO/internal/staging"
)

func damageClosedCache(t *testing.T, s *Service, damage string) {
	t.Helper()
	if damage == "logical" {
		if _, err := s.Store.DB.Exec("UPDATE assets SET metadata = '{}'"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(s.Dir, "index.db")
	switch damage {
	case "physical":
		if err := os.WriteFile(p, []byte("physically damaged cache"), 0600); err != nil {
			t.Fatal(err)
		}
	case "deleted":
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReindexRecoversCacheWithoutLosingStagedWork(t *testing.T) {
	for _, damage := range []string{"deleted", "physical", "logical"} {
		t.Run(damage, func(t *testing.T) {
			ctx, r, s := realRepository(t)
			committed := stageImage(t, ctx, s)
			result := mustCommit(t, ctx, s, r)
			staged := stageImage(t, ctx, s)
			before, err := s.Store.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			payload := staging.PayloadPath(s.Dir, staged.Namespace, staged.Object)
			wantPayload, err := os.ReadFile(payload)
			if err != nil {
				t.Fatal(err)
			}
			damageClosedCache(t, s, damage)
			rebuilt, err := Reindex(ctx, s.Dir, r)
			if err != nil || rebuilt.Head != result.Head || rebuilt.AssetCount != 1 {
				t.Fatalf("reindex failed: %#v, %v", rebuilt, err)
			}
			reopened, err := Open(ctx, s.Dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			after, err := reopened.Store.Load(ctx)
			if err != nil || !reflect.DeepEqual(after.Assets[committed.ID], committed) || !reflect.DeepEqual(after.Changes, before.Changes) || after.BaseHead != before.BaseHead {
				t.Fatalf("recovery lost committed or staged work: %#v, %v", after, err)
			}
			gotPayload, err := os.ReadFile(payload)
			if err != nil || !bytes.Equal(gotPayload, wantPayload) {
				t.Fatal("reindex changed staged payload", err)
			}
			if damage == "physical" {
				backups, err := filepath.Glob(filepath.Join(s.Dir, "index-corrupt-*", "index.db"))
				if err != nil || len(backups) != 1 {
					t.Fatalf("corrupt original not preserved: %v, %v", backups, err)
				}
				original, err := os.ReadFile(backups[0])
				if err != nil || string(original) != "physically damaged cache" {
					t.Fatal("backup differs from corrupt original", err)
				}
			}
			// The preserved staged batch remains a real publishable change.
			published, err := reopened.Commit(ctx, r, "publish preserved stage")
			if err != nil || published.AssetCount != 2 {
				t.Fatalf("recovered work could not publish: %#v, %v", published, err)
			}
		})
	}
}

func TestRunningReindexRepairsLogicalCacheInPlace(t *testing.T) {
	ctx, r, s := realRepository(t)
	asset := stageImage(t, ctx, s)
	mustCommit(t, ctx, s, r)
	stageImage(t, ctx, s)
	before, err := s.Store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Stat(filepath.Join(s.Dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec("UPDATE repository_state SET manifest = '{}'; UPDATE assets SET metadata = '{}'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Load(ctx); !errors.Is(err, domain.ErrRepositoryCorrupt) {
		t.Fatalf("expected logical cache damage: %v", err)
	}
	if _, err := s.Reindex(ctx, r); err != nil {
		t.Fatal(err)
	}
	after, err := s.Store.Load(ctx)
	if err != nil || !reflect.DeepEqual(after.Assets[asset.ID], asset) || !reflect.DeepEqual(after.Changes, before.Changes) {
		t.Fatalf("running connection did not recover: %#v, %v", after, err)
	}
	nextFile, err := os.Stat(filepath.Join(s.Dir, "index.db"))
	if err != nil || !os.SameFile(file, nextFile) {
		t.Fatal("running reindex replaced the database file", err)
	}
}

func TestReindexPreservesJournalUntilBackendSyncRecovery(t *testing.T) {
	for _, outcome := range []string{"unpublished", "published"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, r, s := realRepository(t)
			asset := stageImage(t, ctx, s)
			fault := &publicationFault{Git: r, fail: "ref"}
			if outcome == "published" {
				fault.fail, fault.lost = "", true
				fault.after = func() { fault.unknown = true }
			}
			if _, err := s.Commit(ctx, fault, "pending publication"); !errors.Is(err, errPublicationFault) {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(s.Dir, "workspace.json"))
			if err != nil {
				t.Fatal(err)
			}
			damageClosedCache(t, s, "physical")
			if _, err := Reindex(ctx, s.Dir, r); err != nil {
				t.Fatalf("journal-preserving rebuild failed: %v", err)
			}
			after, err := os.ReadFile(filepath.Join(s.Dir, "workspace.json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("reindex changed pending durable work", err)
			}
			reopened, err := Open(ctx, s.Dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err := reopened.Edit(ctx, asset.ID.String(), EditOptions{}); !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("reindex bypassed publication mutation guard: %v", err)
			}
			if _, err := reopened.Sync(ctx, r); err != nil {
				t.Fatalf("backend could not recover journal after rebuild: %v", err)
			}
			state, err := reopened.Store.Load(ctx)
			if err != nil || state.Publication != nil {
				t.Fatalf("journal recovery incomplete: %#v, %v", state, err)
			}
			if outcome == "published" {
				if len(state.Changes) != 0 || !reflect.DeepEqual(state.Assets[asset.ID], asset) {
					t.Fatal("published batch not finalized", state)
				}
			} else {
				if _, ok := state.Changes[asset.ID]; !ok {
					t.Fatal("unpublished staged batch lost", state)
				}
				if result, err := reopened.Commit(ctx, r, "retry preserved work"); err != nil || result.AssetCount != 1 {
					t.Fatalf("unpublished batch no longer publishable: %#v, %v", result, err)
				}
			}
		})
	}
}

func TestReindexRejectsAdvancedHeadWithStaging(t *testing.T) {
	ctx, r, s := realRepository(t)
	stageImage(t, ctx, s)
	before, err := s.Store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	publishRaw(t, ctx, r, []repository.TreeChange{{Path: "README.md", Blob: blob(t, ctx, r, []byte("advanced"))}})
	damageClosedCache(t, s, "deleted")
	_, err = Reindex(ctx, s.Dir, r)
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) || conflict.Base != before.BaseHead || conflict.Reason != "head_changed" {
		t.Fatalf("expected staged-base conflict: %v", err)
	}
	after, err := index.LoadWorkspace(ctx, s.Dir)
	if err != nil || !reflect.DeepEqual(after.Changes, before.Changes) || after.BaseHead != before.BaseHead {
		t.Fatal("conflict lost durable work", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "index.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("conflict must not construct a new cache", err)
	}
}

func TestReindexBadSourcePreservesOriginalCache(t *testing.T) {
	for _, damage := range []string{"healthy", "physical", "deleted"} {
		t.Run(damage, func(t *testing.T) {
			ctx, r, s := realRepository(t)
			stageImage(t, ctx, s)
			mustCommit(t, ctx, s, r)
			publishRaw(t, ctx, r, []repository.TreeChange{{Path: repository.ManifestPath, Blob: blob(t, ctx, r, []byte("invalid manifest"))}})
			damageClosedCache(t, s, damage)
			p := filepath.Join(s.Dir, "index.db")
			before, beforeErr := os.ReadFile(p)
			workspace, err := os.ReadFile(filepath.Join(s.Dir, "workspace.json"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Reindex(ctx, s.Dir, r); !errors.Is(err, domain.ErrRepositoryCorrupt) {
				t.Fatalf("bad source accepted: %v", err)
			}
			after, afterErr := os.ReadFile(p)
			if !bytes.Equal(before, after) || errors.Is(beforeErr, os.ErrNotExist) != errors.Is(afterErr, os.ErrNotExist) {
				t.Fatal("bad source changed original cache")
			}
			afterWorkspace, err := os.ReadFile(filepath.Join(s.Dir, "workspace.json"))
			if err != nil || !bytes.Equal(workspace, afterWorkspace) {
				t.Fatal("bad source changed durable work", err)
			}
		})
	}
}

type movingReindexHead struct {
	repository.Git
	afterHead func()
}

func (r *movingReindexHead) Head(ctx context.Context) (domain.RepositoryHead, error) {
	head, err := r.Git.Head(ctx)
	if err == nil && r.afterHead != nil {
		fn := r.afterHead
		r.afterHead = nil
		fn()
	}
	return head, err
}

func TestReindexReadsOneImmutableSnapshot(t *testing.T) {
	ctx, r, s := realRepository(t)
	head, err := r.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	moving := &movingReindexHead{Git: r, afterHead: func() {
		publishRaw(t, ctx, r, []repository.TreeChange{{Path: repository.ManifestPath, Blob: blob(t, ctx, r, []byte("new head has bad metadata"))}})
	}}
	result, err := s.Reindex(ctx, moving)
	if err != nil || result.Head != head {
		t.Fatalf("reindex followed moving HEAD: %#v, %v", result, err)
	}
	state, err := s.Store.Load(ctx)
	if err != nil || state.Head != head || state.Manifest == nil {
		t.Fatalf("fixed snapshot not installed: %#v, %v", state, err)
	}
}

func TestReindexBadPayloadDoesNotReplaceCommittedCache(t *testing.T) {
	ctx, r, s := realRepository(t)
	asset := stageImage(t, ctx, s)
	committed := mustCommit(t, ctx, s, r)
	data := pngBytes(t)
	data[len(data)-1] ^= 1
	publishRaw(t, ctx, r, []repository.TreeChange{{Path: repository.ObjectPath(asset), Blob: blob(t, ctx, r, data)}})
	if _, err := s.Reindex(ctx, r); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("payload corruption not rejected: %v", err)
	}
	state, err := s.Store.Load(ctx)
	if err != nil || state.Head != committed.Head || !reflect.DeepEqual(state.Assets[asset.ID], asset) {
		t.Fatalf("bad payload replaced committed cache: %#v, %v", state, err)
	}
}

func TestReindexDeletedCacheStillChecksRepositoryIdentity(t *testing.T) {
	ctx, r, s := realRepository(t)
	otherID, err := domain.NewRepositoryID()
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := repository.EncodeManifest(domain.Repository{ID: otherID, FormatVersion: 1, HashAlgorithm: "sha256"})
	if err != nil {
		t.Fatal(err)
	}
	publishRaw(t, ctx, r, []repository.TreeChange{{Path: repository.ManifestPath, Blob: blob(t, ctx, r, manifest)}})
	damageClosedCache(t, s, "deleted")
	if _, err := Reindex(ctx, s.Dir, r); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("deleted cache lost repository identity protection: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "index.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("identity conflict created replacement cache", err)
	}
}

func TestReindexInitializationJournalRemainsRecoverable(t *testing.T) {
	for _, base := range []string{"empty", "raw"} {
		for _, outcome := range []string{"unpublished", "published"} {
			t.Run(base+"/"+outcome, func(t *testing.T) {
				ctx := context.Background()
				r, err := localgit.Init(ctx, filepath.Join(t.TempDir(), "gallery.git"), "main")
				if err != nil {
					t.Fatal(err)
				}
				if base == "raw" {
					tree, err := r.CreateTree(ctx, "", []repository.TreeChange{{Path: "README.md", Blob: blob(t, ctx, r, []byte("existing image repository"))}})
					if err != nil {
						t.Fatal(err)
					}
					head, err := r.CreateCommit(ctx, tree, "", "existing repository")
					if err != nil {
						t.Fatal(err)
					}
					if err := r.UpdateRef(ctx, "", head); err != nil {
						t.Fatal(err)
					}
				}
				s, err := Open(ctx, t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				fault := &publicationFault{Git: r, fail: "ref"}
				if outcome == "published" {
					fault.fail, fault.lost = "", true
					fault.after = func() { fault.unknown = true }
				}
				if _, err := s.Initialize(ctx, fault); !errors.Is(err, errPublicationFault) {
					t.Fatal(err)
				}
				before, err := os.ReadFile(filepath.Join(s.Dir, "workspace.json"))
				if err != nil {
					t.Fatal(err)
				}
				damageClosedCache(t, s, "deleted")
				if _, err := Reindex(ctx, s.Dir, r); err != nil {
					t.Fatalf("initialization journal cache recovery failed: %v", err)
				}
				after, err := os.ReadFile(filepath.Join(s.Dir, "workspace.json"))
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("initialization journal changed during reindex", err)
				}
				reopened, err := Open(ctx, s.Dir)
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				state, err := reopened.Store.Load(ctx)
				if err != nil || state.Manifest != nil || state.Publication == nil {
					t.Fatalf("initialization outcome was inferred from clone: %#v, %v", state, err)
				}
				if _, err := reopened.Sync(ctx, r); err != nil {
					t.Fatalf("initialization journal cannot recover after reindex: %v", err)
				}
				if _, err := reopened.Initialize(ctx, r); err != nil {
					t.Fatal(err)
				}
				state, err = reopened.Store.Load(ctx)
				if err != nil || state.Publication != nil || state.Manifest == nil || len(state.Changes) != 0 {
					t.Fatalf("initialization failed to finish: %#v, %v", state, err)
				}
			})
		}
	}
}
