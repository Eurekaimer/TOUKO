package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/index"
	"github.com/Eurekaimer/TOUKO/internal/localgit"
	"github.com/Eurekaimer/TOUKO/internal/repository"
	"github.com/Eurekaimer/TOUKO/internal/staging"
)

var errPublicationFault = errors.New("injected publication failure")

type publicationFault struct {
	repository.Git
	fail      string
	lost      bool
	unknown   bool
	after     func()
	beforeRef func()
}

func (f *publicationFault) CreateBlob(ctx context.Context, r io.Reader, size int64) (repository.BlobID, error) {
	if f.fail == "blob" {
		return "", errPublicationFault
	}
	return f.Git.CreateBlob(ctx, r, size)
}
func (f *publicationFault) CreateTree(ctx context.Context, base repository.TreeID, changes []repository.TreeChange) (repository.TreeID, error) {
	if f.fail == "tree" {
		return "", errPublicationFault
	}
	return f.Git.CreateTree(ctx, base, changes)
}
func (f *publicationFault) CreateCommit(ctx context.Context, tree repository.TreeID, base domain.RepositoryHead, message string) (domain.RepositoryHead, error) {
	if f.fail == "commit" {
		return "", errPublicationFault
	}
	return f.Git.CreateCommit(ctx, tree, base, message)
}
func (f *publicationFault) Head(ctx context.Context) (domain.RepositoryHead, error) {
	if f.unknown {
		return "", errPublicationFault
	}
	return f.Git.Head(ctx)
}
func (f *publicationFault) UpdateRef(ctx context.Context, base, next domain.RepositoryHead) error {
	if f.beforeRef != nil {
		fn := f.beforeRef
		f.beforeRef = nil
		fn()
	}
	if f.fail == "ref" {
		return errPublicationFault
	}
	if err := f.Git.UpdateRef(ctx, base, next); err != nil {
		return err
	}
	if f.after != nil {
		f.after()
	}
	if f.lost {
		return errPublicationFault
	}
	return nil
}
func stageImage(t *testing.T, ctx context.Context, s *Service) domain.Asset {
	t.Helper()
	v, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(pngBytes(t)), Name: "截图.png"}}, AddOptions{Namespace: "blog", Tags: []string{"demo"}, Collections: []string{"samples"}})
	if err != nil {
		t.Fatal(err)
	}
	return v[0].Asset
}
func secondService(t *testing.T, ctx context.Context, r repository.Git) *Service {
	t.Helper()
	s, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err = s.Sync(ctx, r); err != nil {
		t.Fatal(err)
	}
	return s
}
func mustCommit(t *testing.T, ctx context.Context, s *Service, r repository.Git) CommitResult {
	t.Helper()
	v, err := s.Commit(ctx, r, "test batch")
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func TestDeleteModifyConflictDoesNotResurrect(t *testing.T) {
	ctx, r, a := realRepository(t)
	asset := stageImage(t, ctx, a)
	mustCommit(t, ctx, a, r)
	b := secondService(t, ctx, r)
	title := "stale edit"
	if err := b.Edit(ctx, asset.ID.String(), EditOptions{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if err := a.Remove(ctx, []string{asset.ID.String()}); err != nil {
		t.Fatal(err)
	}
	head := mustCommit(t, ctx, a, r).Head
	_, err := b.Commit(ctx, r, "must not restore")
	var conflict *domain.ConflictError
	if !errors.Is(err, domain.ErrConflict) || !errors.As(err, &conflict) || conflict.Reason != "delete_modify" {
		t.Fatal(err)
	}
	current, _ := r.Head(ctx)
	snap, err := repository.ReadSnapshot(ctx, r, current)
	if err != nil || current != head || len(snap.Assets) != 0 {
		t.Fatal(current, snap, err)
	}
	fresh := secondService(t, ctx, r)
	st, _ := fresh.Store.Load(ctx)
	if len(st.Assets) != 0 {
		t.Fatal(st.Assets)
	}
}
func TestDeleteFailureBeforePublicationLeavesHeadUnchanged(t *testing.T) {
	for _, point := range []string{"blob", "tree", "commit", "ref"} {
		t.Run(point, func(t *testing.T) {
			ctx, r, s := realRepository(t)
			doomed := stageImage(t, ctx, s)
			mustCommit(t, ctx, s, r)
			base, _ := r.Head(ctx)
			if err := s.Remove(ctx, []string{doomed.ID.String()}); err != nil {
				t.Fatal(err)
			}
			added := stageImage(t, ctx, s)
			f := &publicationFault{Git: r, fail: point}
			if _, err := s.Commit(ctx, f, "replace"); !errors.Is(err, errPublicationFault) {
				t.Fatal(err)
			}
			head, _ := r.Head(ctx)
			st, _ := s.Store.Load(ctx)
			if head != base || st.Head != base || len(st.Changes) != 2 {
				t.Fatal(head, st)
			}
			candidate := domain.RepositoryHead("")
			if st.Publication != nil {
				candidate = st.Publication.CandidateHead
			}
			result := mustCommit(t, ctx, s, r)
			if candidate != "" && result.Head != candidate {
				t.Fatal("retry replaced candidate")
			}
			commit, _ := r.Commit(ctx, result.Head)
			if !reflect.DeepEqual(commit.Parents, []domain.RepositoryHead{base}) {
				t.Fatal(commit)
			}
			snap, err := repository.ReadSnapshot(ctx, r, result.Head)
			if err != nil || len(snap.Assets) != 1 || !reflect.DeepEqual(snap.Assets[added.ID], added) {
				t.Fatal(snap, err)
			}
		})
	}
}
func TestRecoverLostUpdateRefResponse(t *testing.T) {
	for _, descendant := range []bool{false, true} {
		t.Run(fmt.Sprint(descendant), func(t *testing.T) {
			ctx, r, s := realRepository(t)
			asset := stageImage(t, ctx, s)
			f := &publicationFault{Git: r, lost: true}
			if descendant {
				f.after = func() {
					publishRaw(t, ctx, r, []repository.TreeChange{{Path: "README.md", Blob: blob(t, ctx, r, []byte("preserved descendant"))}})
				}
			}
			if _, err := s.Commit(ctx, f, "lost response"); err != nil {
				t.Fatal(err)
			}
			head, _ := r.Head(ctx)
			if _, err := s.Sync(ctx, r); err != nil {
				t.Fatal(err)
			}
			after, _ := r.Head(ctx)
			st, _ := s.Store.Load(ctx)
			if after != head || st.Head != head || st.Publication != nil || len(st.Changes) != 0 || !reflect.DeepEqual(st.Assets[asset.ID], asset) {
				t.Fatal(st)
			}
		})
	}
}
func TestRecoverSQLiteFailureAfterRemoteCommit(t *testing.T) {
	ctx, r, s := realRepository(t)
	asset := stageImage(t, ctx, s)
	base, _ := r.Head(ctx)
	if _, err := s.Store.DB.ExecContext(ctx, `CREATE TRIGGER fail_head BEFORE UPDATE OF head ON repository_state BEGIN SELECT RAISE(ABORT, 'head failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(ctx, r, "published but not indexed"); err == nil {
		t.Fatal("expected SQLite failure")
	}
	head, _ := r.Head(ctx)
	st, _ := s.Store.Load(ctx)
	if head == base || st.Head != base || st.Publication == nil || len(st.Changes) != 1 {
		t.Fatal(st)
	}
	if _, err := s.Store.DB.ExecContext(ctx, `DROP TRIGGER fail_head`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sync(ctx, r); err != nil {
		t.Fatal(err)
	}
	after, _ := r.Head(ctx)
	st, _ = s.Store.Load(ctx)
	if after != head || st.Head != head || st.Publication != nil || len(st.Changes) != 0 || !reflect.DeepEqual(st.Assets[asset.ID], asset) {
		t.Fatal(st)
	}
}
func TestConcurrentCommitDoesNotOverwrite(t *testing.T) {
	ctx, r, a := realRepository(t)
	b := secondService(t, ctx, r)
	loser := stageImage(t, ctx, a)
	winner := stageImage(t, ctx, b)
	f := &publicationFault{Git: r, beforeRef: func() { mustCommit(t, ctx, b, r) }}
	if _, err := a.Commit(ctx, f, "loser"); !errors.Is(err, domain.ErrConflict) {
		t.Fatal(err)
	}
	head, _ := r.Head(ctx)
	snap, err := repository.ReadSnapshot(ctx, r, head)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snap.Assets[loser.ID]; ok {
		t.Fatal("sibling overwritten")
	}
	if !reflect.DeepEqual(snap.Assets[winner.ID], winner) || len(snap.Assets) != 1 {
		t.Fatal(snap)
	}
}
func TestCommitRejectsDamagedStageBeforePublication(t *testing.T) {
	ctx, r, s := realRepository(t)
	asset := stageImage(t, ctx, s)
	base, _ := r.Head(ctx)
	if err := os.WriteFile(staging.PayloadPath(s.Dir, asset.Namespace, asset.Object), bytes.Repeat([]byte{'x'}, int(asset.Object.Size)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(ctx, r, "damaged"); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatal(err)
	}
	head, _ := r.Head(ctx)
	st, _ := s.Store.Load(ctx)
	if head != base || st.Head != base || len(st.Changes) != 1 || st.Publication != nil {
		t.Fatal(st)
	}
}
func TestJournalBaseSyncClearsButCommitRetriesCandidate(t *testing.T) {
	ctx, r, s := realRepository(t)
	stageImage(t, ctx, s)
	f := &publicationFault{Git: r, fail: "ref"}
	if _, err := s.Commit(ctx, f, "retry"); err == nil {
		t.Fatal("expected failure")
	}
	st, _ := s.Store.Load(ctx)
	candidate := st.Publication.CandidateHead
	if _, err := s.Sync(ctx, r); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Store.Load(ctx)
	if st.Publication != nil || len(st.Changes) != 1 {
		t.Fatal(st)
	}
	// Restore the same valid journal to prove commit retries immutable work.
	commit, _ := r.Commit(ctx, candidate)
	if err := s.Store.SavePublication(ctx, &index.Publication{BaseHead: st.Head, CandidateHead: candidate, CandidateTree: string(commit.Tree)}); err != nil {
		t.Fatal(err)
	}
	if got := mustCommit(t, ctx, s, r).Head; got != candidate {
		t.Fatal(got, candidate)
	}
}
func TestUnknownPublicationOutcomeRetainsJournal(t *testing.T) {
	ctx, r, s := realRepository(t)
	stageImage(t, ctx, s)
	f := &publicationFault{Git: r, lost: true}
	f.after = func() { f.unknown = true }
	_, err := s.Commit(ctx, f, "unknown")
	st, _ := s.Store.Load(ctx)
	if err == nil || st.Publication == nil || !strings.Contains(err.Error(), string(st.Publication.CandidateHead)) || !strings.Contains(err.Error(), "run sync") || len(st.Changes) != 1 {
		t.Fatal(st, err)
	}
	if _, err = s.Sync(ctx, r); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Store.Load(ctx)
	if st.Publication != nil || len(st.Changes) != 0 || len(st.Assets) != 1 {
		t.Fatal(st)
	}
}
func TestUnrelatedJournalConflictAndExplicitReset(t *testing.T) {
	ctx, r, s := realRepository(t)
	stageImage(t, ctx, s)
	f := &publicationFault{Git: r, fail: "ref"}
	if _, err := s.Commit(ctx, f, "candidate"); err == nil {
		t.Fatal("expected failure")
	}
	other := secondService(t, ctx, r)
	winner := stageImage(t, ctx, other)
	head := mustCommit(t, ctx, other, r).Head
	if _, err := s.Sync(ctx, r); !errors.Is(err, domain.ErrConflict) {
		t.Fatal(err)
	}
	st, _ := s.Store.Load(ctx)
	if st.Publication == nil || len(st.Changes) != 1 {
		t.Fatal(st)
	}
	if err := s.Reset(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sync(ctx, r); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Store.Load(ctx)
	if st.Head != head || st.Publication != nil || len(st.Changes) != 0 || len(st.Assets) != 1 || !reflect.DeepEqual(st.Assets[winner.ID], winner) {
		t.Fatal(st)
	}
}
func TestInitializeRecoversJournal(t *testing.T) {
	ctx := context.Background()
	r, err := localgit.Init(ctx, filepath.Join(t.TempDir(), "init.git"), "main")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f := &publicationFault{Git: r, fail: "ref"}
	if _, err = s.Initialize(ctx, f); err == nil {
		t.Fatal("expected failure")
	}
	st, _ := s.Store.Load(ctx)
	if st.Publication == nil {
		t.Fatal(st)
	}
	candidate := st.Publication.CandidateHead
	result, err := s.Initialize(ctx, r)
	if err != nil || result.Head != candidate {
		t.Fatal(result, err)
	}
}

func TestRecoveryRejectsCandidateMetadataAndDeletionCorruption(t *testing.T) {
	for _, mode := range []string{"metadata", "deleted_object", "unrelated_file"} {
		t.Run(mode, func(t *testing.T) {
			ctx, r, s := realRepository(t)
			asset := stageImage(t, ctx, s)
			mustCommit(t, ctx, s, r)
			base, _ := r.Head(ctx)
			baseSnapshot, err := repository.ReadSnapshot(ctx, r, base)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "metadata" {
				title := "expected title"
				if err = s.Edit(ctx, asset.ID.String(), EditOptions{Title: &title}); err != nil {
					t.Fatal(err)
				}
			} else {
				if err = s.Remove(ctx, []string{asset.ID.String()}); err != nil {
					t.Fatal(err)
				}
			}
			f := &publicationFault{Git: r, fail: "ref"}
			if _, err = s.Commit(ctx, f, "unpublished valid candidate"); err == nil {
				t.Fatal("expected ref failure")
			}
			st, _ := s.Store.Load(ctx)
			candidate, err := r.Commit(ctx, st.Publication.CandidateHead)
			if err != nil {
				t.Fatal(err)
			}
			var changes []repository.TreeChange
			switch mode {
			case "metadata":
				wrong := asset
				wrong.Title = "not staged"
				data, e := repository.EncodeAsset(wrong)
				if e != nil {
					t.Fatal(e)
				}
				changes = []repository.TreeChange{{Path: repository.AssetPath(asset.ID), Blob: blob(t, ctx, r, data)}}
			case "deleted_object":
				path := repository.BuildObjectPath(asset.Namespace, asset.Object)
				for _, entry := range baseSnapshot.Entries {
					if entry.Path == path {
						id := repository.BlobID(entry.SHA)
						changes = append(changes, repository.TreeChange{Path: path, Blob: &id})
					}
				}
			case "unrelated_file":
				changes = []repository.TreeChange{{Path: "unexpected.txt", Blob: blob(t, ctx, r, []byte("must not be published by batch"))}}
			}
			tree, err := r.CreateTree(ctx, candidate.Tree, changes)
			if err != nil {
				t.Fatal(err)
			}
			bad, err := r.CreateCommit(ctx, tree, base, "corrupt recovered candidate")
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Store.SavePublication(ctx, &index.Publication{BaseHead: base, CandidateHead: bad, CandidateTree: string(tree)}); err != nil {
				t.Fatal(err)
			}
			if err = r.UpdateRef(ctx, base, bad); err != nil {
				t.Fatal(err)
			}
			if _, err = s.Sync(ctx, r); !errors.Is(err, domain.ErrRepositoryCorrupt) {
				t.Fatal(err)
			}
			after, _ := s.Store.Load(ctx)
			if after.Head != base || after.Publication == nil || len(after.Changes) != 1 || !reflect.DeepEqual(after.Assets, st.Assets) {
				t.Fatal(after)
			}
		})
	}
}

func TestResetFinalizesPublishedCandidateAndRefusesUnknownOutcome(t *testing.T) {
	ctx, r, s := realRepository(t)
	asset := stageImage(t, ctx, s)
	f := &publicationFault{Git: r, lost: true}
	f.after = func() { f.unknown = true }
	if _, err := s.Commit(ctx, f, "unknown"); err == nil {
		t.Fatal("expected unknown outcome")
	}
	if err := s.Reset(ctx, f); err == nil {
		t.Fatal("reset discarded uncertain journal")
	}
	st, _ := s.Store.Load(ctx)
	if st.Publication == nil || len(st.Changes) != 1 {
		t.Fatal(st)
	}
	head, _ := r.Head(ctx)
	if err := s.Reset(ctx, r); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Store.Load(ctx)
	if st.Head != head || st.Publication != nil || len(st.Changes) != 0 || !reflect.DeepEqual(st.Assets[asset.ID], asset) {
		t.Fatal(st)
	}
}

func TestSyncRetainsStageOnlyAtUnchangedHead(t *testing.T) {
	ctx, r, s := realRepository(t)
	asset := stageImage(t, ctx, s)
	if _, err := s.Sync(ctx, r); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Store.Load(ctx)
	if len(before.Changes) != 1 || before.BaseHead != before.Head {
		t.Fatal(before)
	}
	other := secondService(t, ctx, r)
	stageImage(t, ctx, other)
	mustCommit(t, ctx, other, r)
	if _, err := s.Sync(ctx, r); !errors.Is(err, domain.ErrConflict) {
		t.Fatal(err)
	}
	after, _ := s.Store.Load(ctx)
	if after.Head != before.Head || after.BaseHead != before.BaseHead || !reflect.DeepEqual(after.Changes, before.Changes) {
		t.Fatal(after)
	}
	if err := s.Reset(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sync(ctx, r); err != nil {
		t.Fatal(err)
	}
	after, _ = s.Store.Load(ctx)
	if _, ok := after.Assets[asset.ID]; ok {
		t.Fatal("unpublished add leaked into remote")
	}
}

func TestInitializeRecoversPublishedAfterSQLiteFailure(t *testing.T) {
	ctx := context.Background()
	r, err := localgit.Init(ctx, filepath.Join(t.TempDir(), "init.git"), "main")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Store.DB.ExecContext(ctx, `CREATE TRIGGER fail_init_head BEFORE UPDATE OF head ON repository_state BEGIN SELECT RAISE(ABORT, 'init head failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Initialize(ctx, r); err == nil {
		t.Fatal("expected local failure")
	}
	head, _ := r.Head(ctx)
	st, _ := s.Store.Load(ctx)
	if head == "" || st.Manifest != nil || st.Publication == nil {
		t.Fatal(st)
	}
	if _, err = s.Store.DB.ExecContext(ctx, `DROP TRIGGER fail_init_head`); err != nil {
		t.Fatal(err)
	}
	result, err := s.Initialize(ctx, r)
	if err != nil || result.Head != head {
		t.Fatal(result, err)
	}
	after, _ := r.Head(ctx)
	st, _ = s.Store.Load(ctx)
	if after != head || st.Head != head || st.Manifest == nil || st.Publication != nil {
		t.Fatal(st)
	}
}

func TestResetWithoutJournalWorksOffline(t *testing.T) {
	ctx, r, s := realRepository(t)
	asset := stageImage(t, ctx, s)
	before, err := s.Store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.Publication != nil {
		t.Fatal("unexpected publication journal")
	}
	if err = s.Reset(ctx, &publicationFault{Git: r, unknown: true}); err != nil {
		t.Fatal(err)
	}
	after, err := s.Store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Head != before.Head || after.BaseHead != "" || len(after.Changes) != 0 || after.Publication != nil || !reflect.DeepEqual(after.Assets, before.Assets) {
		t.Fatal(after)
	}
	if _, err = os.Stat(staging.PayloadPath(s.Dir, asset.Namespace, asset.Object)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unreferenced staged payload retained", err)
	}
}

func TestCloudflareUnavailableStillPublishes(t *testing.T) {
	ctx, r, s := realRepository(t)
	config := `{"backend":"github","owner":"owner","name":"gallery","branch":"main","cloudflare":{"enabled":true,"endpoint":"https://acct.r2.cloudflarestorage.com","bucket":"images","public_base_url":"https://img.example.test"}}`
	if err := os.WriteFile(filepath.Join(s.Dir, "config.json"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	asset := stageImage(t, ctx, s)
	result := mustCommit(t, ctx, s, r)
	head, err := r.Head(ctx)
	if err != nil || head != result.Head {
		t.Fatal(head, result.Head, err)
	}
	snapshot, err := repository.ReadSnapshot(ctx, r, head)
	if err != nil || snapshot.Assets[asset.ID].Object != asset.Object {
		t.Fatal(snapshot, err)
	}
	state, err := s.Store.Load(ctx)
	if err != nil || len(state.Changes) != 0 || state.Publication != nil {
		t.Fatal(state, err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Cloudflare *struct {
			Status string `json:"status"`
		} `json:"cloudflare"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	if body.Cloudflare == nil || body.Cloudflare.Status != "unavailable" {
		t.Fatalf("published without explicit unavailable mirror result: %s", data)
	}
}
