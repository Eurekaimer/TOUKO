package index

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/domain"
)

func sampleState(t *testing.T) State {
	t.Helper()
	id, err := domain.ParseAssetID("11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	rid, err := domain.ParseRepositoryID("22222222-2222-4222-8222-222222222222")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 15, 12, 30, 45, 123456789, time.UTC)
	a := domain.Asset{ID: id, Namespace: "blog", Object: domain.ContentObject{Hash: domain.ObjectHash{1, 2, 3}, MIME: "image/png", Ext: "png", Size: 100, Width: 64, Height: 48}, OriginalName: "剪贴板.png", Title: "标题", Tags: []string{"demo", "摄影"}, Collections: []string{"作品"}, CreatedAt: now, UpdatedAt: now}
	updated := a
	updated.Title = "edited title"
	updated.UpdatedAt = now.Add(time.Second)
	return State{Head: "base", Tree: "base-tree", Manifest: &domain.Repository{ID: rid, FormatVersion: 1, HashAlgorithm: "sha256"}, BaseHead: "base", Assets: map[domain.AssetID]domain.Asset{id: a}, Changes: map[domain.AssetID]domain.Change{id: {Kind: domain.Update, AssetID: id, Asset: &updated}}, Publication: &Publication{BaseHead: "base", CandidateHead: "candidate", CandidateTree: "candidate-tree"}}
}

func openTestStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestPersistenceRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Manifest != nil || empty.Head != "" || empty.Assets == nil || empty.Changes == nil || len(empty.Assets) != 0 || len(empty.Changes) != 0 {
		t.Fatalf("unexpected initial state: %#v", empty)
	}
	want := sampleState(t)
	if err := s.Save(ctx, want); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, dir)
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("roundtrip mismatch\ngot: %#v\nwant: %#v", got, want)
	}
	for path, mode := range map[string]os.FileMode{dir: 0700, filepath.Join(dir, "index.db"): 0600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Errorf("%s permissions: %o, want %o", path, info.Mode().Perm(), mode)
		}
	}
	var version, foreignKeys int
	var journal string
	if err := s.DB.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if version != 1 || foreignKeys != 1 || strings.ToLower(journal) != "wal" {
		t.Fatalf("SQLite settings: version=%d foreign_keys=%d journal=%s", version, foreignKeys, journal)
	}
}

func TestSaveRollbackAndIndependentPublication(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, t.TempDir())
	initial := sampleState(t)
	initial.Publication = nil
	if err := s.Save(ctx, initial); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `CREATE TRIGGER fail_head BEFORE UPDATE OF head ON repository_state BEGIN SELECT RAISE(ABORT, 'head failure'); END`); err != nil {
		t.Fatal(err)
	}
	journal := &Publication{BaseHead: initial.Head, CandidateHead: "published", CandidateTree: "published-tree"}
	if err := s.SavePublication(ctx, journal); err != nil {
		t.Fatalf("publication must not update HEAD: %v", err)
	}
	replacement := sampleState(t)
	replacement.Head = journal.CandidateHead
	replacement.Tree = journal.CandidateTree
	replacement.Assets = map[domain.AssetID]domain.Asset{}
	replacement.Changes = map[domain.AssetID]domain.Change{}
	replacement.BaseHead = ""
	replacement.Publication = nil
	if err := s.Save(ctx, replacement); err == nil {
		t.Fatal("expected trigger failure")
	}
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	initial.Publication = journal
	if !reflect.DeepEqual(got, initial) {
		t.Fatalf("failed transaction replaced state: %#v", got)
	}
	if err := s.SavePublication(ctx, nil); err != nil {
		t.Fatal(err)
	}
	got, err = s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	initial.Publication = nil
	if !reflect.DeepEqual(got, initial) {
		t.Fatalf("deleting journal changed other state: %#v", got)
	}
}

func TestLoadRejectsCorruption(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"unknown change", `UPDATE changes SET kind = 'restore'`},
		{"missing staged metadata", `UPDATE changes SET metadata = NULL`},
		{"delete carrying metadata", `UPDATE changes SET kind = 'delete'`},
		{"invalid committed metadata", `UPDATE assets SET metadata = '{}'`},
		{"mismatched committed ID", `UPDATE assets SET id = '33333333-3333-4333-8333-333333333333'`},
		{"mismatched staged ID", `UPDATE changes SET asset_id = '33333333-3333-4333-8333-333333333333'`},
		{"unknown manifest version", `UPDATE repository_state SET manifest = '{"format_version":99,"repository_id":"22222222-2222-4222-8222-222222222222","hash_algorithm":"sha256"}'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := openTestStore(t, t.TempDir())
			if err := s.Save(ctx, sampleState(t)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB.ExecContext(ctx, tc.sql); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Load(ctx); !errors.Is(err, domain.ErrRepositoryCorrupt) {
				t.Fatalf("expected corruption, got %v", err)
			}
		})
	}
}

func TestChangeKindsSurviveReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	state := sampleState(t)
	for id, change := range state.Changes {
		change.Kind, change.Asset = domain.Delete, nil
		state.Changes[id] = change
	}
	addedID, err := domain.ParseAssetID("33333333-3333-4333-8333-333333333333")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range state.Assets {
		a.ID = addedID
		state.Changes[addedID] = domain.Change{Kind: domain.Add, AssetID: addedID, Asset: &a}
	}
	if err := s.Save(ctx, state); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, dir)
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, state) {
		t.Fatalf("changes did not persist: %#v", got.Changes)
	}
}

func TestUnsupportedVersionRejected(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, "PRAGMA user_version = 2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, dir)
	if s != nil {
		s.Close()
	}
	if !errors.Is(err, domain.ErrRepositoryCorrupt) {
		t.Fatalf("expected unsupported version error, got %v", err)
	}
}

func TestInvalidSavePreservesState(t *testing.T) {
	for _, kind := range []domain.ChangeKind{domain.Add, domain.Update, domain.Delete, "restore"} {
		t.Run(string(kind), func(t *testing.T) {
			ctx := context.Background()
			s := openTestStore(t, t.TempDir())
			initial := sampleState(t)
			if err := s.Save(ctx, initial); err != nil {
				t.Fatal(err)
			}
			invalid := sampleState(t)
			invalid.Head = "must-not-be-saved"
			invalid.Assets = map[domain.AssetID]domain.Asset{}
			for id, change := range invalid.Changes {
				change.Kind = kind
				if kind == domain.Add || kind == domain.Update {
					change.Asset = nil
				}
				invalid.Changes[id] = change
			}
			if err := s.Save(ctx, invalid); !errors.Is(err, domain.ErrRepositoryCorrupt) {
				t.Fatalf("expected invalid change rejection, got %v", err)
			}
			got, err := s.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, initial) {
				t.Fatalf("invalid save replaced state: %#v", got)
			}
		})
	}
}

func TestFailedJournalReplacementPreservesPublication(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, t.TempDir())
	initial := sampleState(t)
	if err := s.Save(ctx, initial); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `CREATE TRIGGER fail_publication BEFORE INSERT ON publication BEGIN SELECT RAISE(ABORT, 'journal failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePublication(ctx, &Publication{BaseHead: "base", CandidateHead: "other", CandidateTree: "other-tree"}); err == nil {
		t.Fatal("expected journal failure")
	}
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, initial) {
		t.Fatalf("failed journal replacement changed state: %#v", got)
	}
}

func TestCanceledSavePreservesState(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, t.TempDir())
	initial := sampleState(t)
	if err := s.Save(ctx, initial); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.Save(canceled, State{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, initial) {
		t.Fatalf("canceled save changed state: %#v", got)
	}
}
