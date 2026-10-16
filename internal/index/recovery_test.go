package index

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

const legacySchema = `
CREATE TABLE repository_state (singleton INTEGER PRIMARY KEY CHECK(singleton=1), head TEXT NOT NULL, tree TEXT NOT NULL, manifest BLOB, staging_base_head TEXT NOT NULL);
CREATE TABLE assets (id TEXT PRIMARY KEY, metadata BLOB NOT NULL);
CREATE TABLE changes (asset_id TEXT PRIMARY KEY, kind TEXT NOT NULL, metadata BLOB);
CREATE TABLE publication (singleton INTEGER PRIMARY KEY CHECK(singleton=1), base_head TEXT NOT NULL, candidate_head TEXT NOT NULL, candidate_tree TEXT NOT NULL);
PRAGMA user_version=1;`

func legacyStore(t *testing.T, dir string, state State) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(legacySchema); err != nil {
		t.Fatal(err)
	}
	manifest, err := repository.EncodeManifest(*state.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO repository_state VALUES (1,?,?,?,?)", state.Head, state.Tree, manifest, state.BaseHead); err != nil {
		t.Fatal(err)
	}
	for id, a := range state.Assets {
		data, err := repository.EncodeAsset(a)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO assets VALUES (?,?)", id.String(), data); err != nil {
			t.Fatal(err)
		}
	}
	for id, c := range state.Changes {
		var data []byte
		if c.Asset != nil {
			data, err = repository.EncodeAsset(*c.Asset)
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.Exec("INSERT INTO changes VALUES (?,?,?)", id.String(), string(c.Kind), data); err != nil {
			t.Fatal(err)
		}
	}
	if p := state.Publication; p != nil {
		if _, err := db.Exec("INSERT INTO publication VALUES (1,?,?,?)", p.BaseHead, p.CandidateHead, p.CandidateTree); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestLegacyMigrationPreservesStageAndPublication(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	want := sampleState(t)
	for _, a := range want.Assets {
		added := a
		added.ID = domain.AssetID{3}
		want.Changes[added.ID] = domain.Change{Kind: domain.Add, AssetID: added.ID, Asset: &added}
		deleted := a
		deleted.ID = domain.AssetID{4}
		want.Assets[deleted.ID] = deleted
		want.Changes[deleted.ID] = domain.Change{Kind: domain.Delete, AssetID: deleted.ID}
		break
	}
	db := legacyStore(t, dir, want)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, dir)
	got, err := s.Load(ctx)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("migration lost state: %#v, %v", got, err)
	}
	var oldTables int
	if err := s.DB.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('changes','publication')").Scan(&oldTables); err != nil || oldTables != 0 {
		t.Fatalf("legacy truth still present: %d, %v", oldTables, err)
	}
	info, err := os.Stat(filepath.Join(dir, workspaceName))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("durable workspace permissions: %v, %v", info, err)
	}
	if err := s.DB.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.db"), []byte("not a database"), 0600); err != nil {
		t.Fatal(err)
	}
	durable, err := LoadWorkspace(ctx, dir)
	if err != nil || !reflect.DeepEqual(durable.Changes, want.Changes) || !reflect.DeepEqual(durable.Publication, want.Publication) || durable.BaseHead != want.BaseHead {
		t.Fatalf("cache damage destroyed durable work: %#v, %v", durable, err)
	}
}

func TestFailedMigrationRetainsLegacySource(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	want := sampleState(t)
	db := legacyStore(t, dir, want)
	defer db.Close()
	if err := os.Mkdir(filepath.Join(dir, workspaceName), 0700); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(ctx, dir); err == nil {
		s.Close()
		t.Fatal("migration must fail when durable copy cannot be installed")
	}
	var version, changes, publications int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM changes").Scan(&changes); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM publication").Scan(&publications); err != nil {
		t.Fatal(err)
	}
	if version != 1 || changes != len(want.Changes) || publications != 1 {
		t.Fatalf("migration failure destroyed source: version=%d changes=%d publications=%d", version, changes, publications)
	}
}

func TestMigrationResumesAfterDurableCopyBeforeSQLiteCommit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	want := sampleState(t)
	db := legacyStore(t, dir, want)
	db.Close()
	if err := saveWorkspace(ctx, dir, want); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, dir)
	got, err := s.Load(ctx)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("interrupted migration lost work: %#v, %v", got, err)
	}
}

func TestLoadRejectsDamagedDurableChanges(t *testing.T) {
	for _, damage := range []string{"unknown kind", "missing metadata", "delete metadata", "mismatched ID"} {
		t.Run(damage, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			s := openTestStore(t, dir)
			want := sampleState(t)
			if err := s.Save(ctx, want); err != nil {
				t.Fatal(err)
			}
			for id, change := range want.Changes {
				switch damage {
				case "unknown kind":
					change.Kind = "restore"
				case "missing metadata":
					change.Asset = nil
				case "delete metadata":
					change.Kind = domain.Delete
				case "mismatched ID":
					change.AssetID = domain.AssetID{9}
				}
				want.Changes[id] = change
			}
			data, err := json.Marshal(workspace{Version: 1, BaseHead: want.BaseHead, Changes: want.Changes, Publication: want.Publication})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, workspaceName), data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Load(ctx); !errors.Is(err, domain.ErrRepositoryCorrupt) {
				t.Fatalf("damaged durable work silently accepted: %v", err)
			}
		})
	}
}

func TestCommittedCacheBeforeJournalClearRemainsRecoverable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openTestStore(t, dir)
	initial := sampleState(t)
	if err := s.Save(ctx, initial); err != nil {
		t.Fatal(err)
	}
	next := initial
	next.Head, next.Tree = initial.Publication.CandidateHead, initial.Publication.CandidateTree
	next.Changes, next.Publication, next.BaseHead = nil, nil, ""
	// Inject failure after the SQLite commit but before workspace replacement.
	s.dir = filepath.Join(dir, "missing-directory")
	if err := s.Save(ctx, next); err == nil {
		t.Fatal("expected durable workspace write failure")
	}
	s.dir = dir
	got, err := s.Load(ctx)
	if err != nil || got.Head != next.Head || !reflect.DeepEqual(got.Changes, initial.Changes) || !reflect.DeepEqual(got.Publication, initial.Publication) {
		t.Fatalf("journal-before-clear recovery broken: %#v, %v", got, err)
	}
	if err := s.Save(ctx, next); err != nil {
		t.Fatal(err)
	}
	got, err = s.Load(ctx)
	if err != nil || got.Publication != nil || len(got.Changes) != 0 || got.BaseHead != "" {
		t.Fatalf("finalization failed: %#v, %v", got, err)
	}
}

func TestEmptyRepositoryPublicationJournalSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := &Publication{CandidateHead: "first-commit", CandidateTree: "first-tree"}
	if err := s.SavePublication(ctx, want); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestStore(t, dir)
	state, err := reopened.Load(ctx)
	if err != nil || !reflect.DeepEqual(state.Publication, want) {
		t.Fatalf("root initialization journal lost: %#v, %v", state, err)
	}
}

func TestDamagedLegacyStageRefusesMigrationWithoutDroppingSource(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := legacyStore(t, dir, sampleState(t))
	defer db.Close()
	if _, err := db.Exec("UPDATE changes SET kind = 'restore'"); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(ctx, dir); !errors.Is(err, domain.ErrRepositoryCorrupt) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("damaged staged batch silently migrated: %v", err)
	}
	var kind string
	if err := db.QueryRow("SELECT kind FROM changes").Scan(&kind); err != nil || kind != "restore" {
		t.Fatalf("damaged source was discarded: %q, %v", kind, err)
	}
	if _, err := os.Stat(filepath.Join(dir, workspaceName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid legacy batch created durable replacement", err)
	}
}

func TestMigrationRefusesDisagreeingDurableCopy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	legacy := sampleState(t)
	db := legacyStore(t, dir, legacy)
	defer db.Close()
	durable := sampleState(t)
	durable.BaseHead = "different-base"
	if err := saveWorkspace(ctx, dir, durable); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, workspaceName))
	if err != nil {
		t.Fatal(err)
	}
	if s, err := Open(ctx, dir); !errors.Is(err, domain.ErrRepositoryCorrupt) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("conflicting durable copies were silently merged: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, workspaceName))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("conflict replaced durable copy", err)
	}
	var base string
	if err := db.QueryRow("SELECT staging_base_head FROM repository_state").Scan(&base); err != nil || base != string(legacy.BaseHead) {
		t.Fatalf("conflict destroyed legacy copy: %q, %v", base, err)
	}
}
