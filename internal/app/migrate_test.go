package app

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

// legacyGallery turns the current repository into one written by an older
// client: objects live at content-addressed paths and metadata has no path.
func legacyGallery(t *testing.T, ctx context.Context, r repository.Git, s *Service) {
	t.Helper()
	views, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range views {
		asset := view.Asset
		oldPath := repository.ObjectPath(asset)
		entry, err := findEntry(ctx, r, oldPath)
		if err != nil {
			t.Fatal(err)
		}
		legacyPath := repository.LegacyObjectPath(asset.Namespace, asset.Object)
		blob := repository.BlobID(entry.SHA)
		asset.Path = ""
		data, err := repository.EncodeAsset(asset)
		if err != nil {
			t.Fatal(err)
		}
		metaBlob, err := r.CreateBlob(ctx, bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		publishRaw(t, ctx, r, []repository.TreeChange{
			{Path: legacyPath, Blob: &blob},
			{Path: repository.AssetPath(asset.ID), Blob: &metaBlob},
			{Path: oldPath},
		})
	}
	if _, err := s.Sync(ctx, r); err != nil {
		t.Fatal(err)
	}
}

func findEntry(ctx context.Context, r repository.Git, path string) (repository.GitEntry, error) {
	head, err := r.Head(ctx)
	if err != nil {
		return repository.GitEntry{}, err
	}
	commit, err := r.Commit(ctx, head)
	if err != nil {
		return repository.GitEntry{}, err
	}
	entries, err := r.Tree(ctx, commit.Tree)
	if err != nil {
		return repository.GitEntry{}, err
	}
	entry, ok := entryAt(entries, path)
	if !ok {
		return repository.GitEntry{}, errors.New("object entry missing: " + path)
	}
	return entry, nil
}

func setGitHubLocation(t *testing.T, s *Service) {
	t.Helper()
	data := []byte(`{"backend":"github","branch":"main","owner":"Owner","name":"Gallery","private":false}` + "\n")
	if err := os.WriteFile(filepath.Join(s.Dir, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func snapshotAt(t *testing.T, ctx context.Context, r repository.Git) repository.Snapshot {
	t.Helper()
	head, err := r.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := repository.ReadSnapshot(ctx, r, head)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestMigratePathsRewritesAndKeepsOldObjects(t *testing.T) {
	ctx, r, s := realRepository(t)
	if _, err := s.Add(ctx, []Input{
		{Reader: bytes.NewReader(batchPNG(t, 11)), Name: "one.png"},
		{Reader: bytes.NewReader(batchPNG(t, 12)), Name: "two.png"},
	}, AddOptions{Namespace: "blog"}); err != nil {
		t.Fatal(err)
	}
	before := mustCommit(t, ctx, s, r).Head
	legacyGallery(t, ctx, r, s)
	legacyHead, err := r.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	legacySnap, err := repository.ReadSnapshot(ctx, r, legacyHead)
	if err != nil {
		t.Fatal(err)
	}
	for _, asset := range legacySnap.Assets {
		if asset.Path != "" || repository.ObjectPath(asset) != repository.LegacyObjectPath(asset.Namespace, asset.Object) {
			t.Fatalf("fixture is not legacy: %#v", asset)
		}
	}
	if before == legacyHead {
		t.Fatal("fixture did not advance")
	}

	// A dry run reports the plan and changes nothing.
	dry, err := s.MigratePaths(ctx, r, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Migrated != 2 || dry.Failed != 0 || dry.Head != legacyHead {
		t.Fatalf("dry run = %#v", dry)
	}
	if head, _ := r.Head(ctx); head != legacyHead {
		t.Fatal("dry run moved the branch")
	}
	if plan, found, planErr := s.readMigrationPlan(ctx); planErr != nil || found {
		t.Fatalf("dry run wrote a migration plan: %#v, %v, %v", plan, found, planErr)
	}
	for _, asset := range legacySnap.Assets {
		if _, entryErr := findEntry(ctx, r, repository.AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext)); entryErr == nil {
			t.Fatalf("dry run wrote a new object: %#v", asset)
		}
	}

	result, err := s.MigratePaths(ctx, r, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Migrated != 2 || result.Failed != 0 || result.Head == legacyHead {
		t.Fatalf("migration = %#v", result)
	}
	snap, err := repository.ReadSnapshot(ctx, r, result.Head)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Manifest.FormatVersion != 3 {
		t.Fatalf("migration must block legacy clients that could delete old R2 keys: manifest version = %d", snap.Manifest.FormatVersion)
	}
	paths := map[string]bool{}
	for _, entry := range snap.Entries {
		paths[entry.Path] = true
	}
	for _, asset := range snap.Assets {
		want := repository.AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext)
		if asset.Path != want {
			t.Fatalf("asset path = %q, want %q", asset.Path, want)
		}
		if !paths[want] {
			t.Fatalf("new object %s missing", want)
		}
		// Old URLs keep working because the legacy object stays in the tree.
		if !paths[repository.LegacyObjectPath(asset.Namespace, asset.Object)] {
			t.Fatalf("legacy object removed before verification: %#v", asset)
		}
	}
	if _, err := s.Verify(ctx, r); err != nil {
		t.Fatalf("migrated gallery does not verify: %v", err)
	}

	pruned, err := s.MigratePaths(ctx, r, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if pruned.Pruned != 2 || pruned.Head == result.Head {
		t.Fatalf("prune = %#v", pruned)
	}
	snap, err = repository.ReadSnapshot(ctx, r, pruned.Head)
	if err != nil {
		t.Fatal(err)
	}
	remaining := map[string]bool{}
	for _, entry := range snap.Entries {
		remaining[entry.Path] = true
	}
	for _, asset := range snap.Assets {
		if !remaining[repository.AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext)] {
			t.Fatalf("prune removed a live object: %#v", asset)
		}
	}
}

func TestMigratePathsFailureKeepsOldLayout(t *testing.T) {
	ctx, r, s := realRepository(t)
	if _, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(batchPNG(t, 21)), Name: "one.png"}}, AddOptions{Namespace: "blog"}); err != nil {
		t.Fatal(err)
	}
	mustCommit(t, ctx, s, r)
	legacyGallery(t, ctx, r, s)
	head, err := r.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MigratePaths(ctx, &publicationFault{Git: r, fail: "tree"}, false, false); err == nil {
		t.Fatal("migration ignored a tree failure")
	}
	if current, _ := r.Head(ctx); current != head {
		t.Fatal("failed migration moved the branch")
	}
	snap, err := repository.ReadSnapshot(ctx, r, head)
	if err != nil {
		t.Fatal(err)
	}
	for _, asset := range snap.Assets {
		if asset.Path != "" {
			t.Fatalf("failed migration rewrote metadata: %#v", asset)
		}
	}
}

func TestMigratePathsRefusesStagedChanges(t *testing.T) {
	ctx, r, s := realRepository(t)
	stageImage(t, ctx, s)
	head, _ := r.Head(ctx)
	_, err := s.MigratePaths(ctx, r, false, false)
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) || conflict.Reason != "staged_changes" {
		t.Fatalf("migration ignored staged work: %v", err)
	}
	st, err := s.Store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Changes) != 1 || st.Head != head || st.Publication != nil {
		t.Fatalf("migration cleared or changed staged work: %#v", st)
	}
	if current, _ := r.Head(ctx); current != head {
		t.Fatal("refused migration moved the branch")
	}
}

func TestMigratePathsRecoversInterruptedObjectsJournal(t *testing.T) {
	ctx, r, s := realRepository(t)
	if _, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(batchPNG(t, 31)), Name: "one.png"}}, AddOptions{Namespace: "blog"}); err != nil {
		t.Fatal(err)
	}
	mustCommit(t, ctx, s, r)
	legacyGallery(t, ctx, r, s)
	if _, err := s.MigratePaths(ctx, &publicationFault{Git: r, fail: "ref"}, false, false); err == nil {
		t.Fatal("expected a refused ref update")
	}
	st, _ := s.Store.Load(ctx)
	if st.Publication == nil || st.Publication.Operation != migrationObjectsOperation {
		t.Fatalf("missing durable objects journal: %#v", st)
	}
	base, candidate := st.Publication.BaseHead, st.Publication.CandidateHead
	if err := r.UpdateRef(ctx, base, candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sync(ctx, r); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Store.Load(ctx)
	if st.Head != candidate || st.Publication != nil {
		t.Fatalf("sync did not finalize the objects journal: %#v", st)
	}
	for _, asset := range st.Assets {
		if asset.Path != "" {
			t.Fatalf("objects phase changed metadata: %#v", asset)
		}
	}
	// Resume: phase one is already visible, so only metadata is published.
	result, err := s.MigratePaths(ctx, r, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Migrated != 1 {
		t.Fatalf("resume = %#v", result)
	}
	snap, err := repository.ReadSnapshot(ctx, r, result.Head)
	if err != nil {
		t.Fatal(err)
	}
	for _, asset := range snap.Assets {
		if asset.Path != repository.AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext) {
			t.Fatalf("resume did not switch metadata: %#v", asset)
		}
	}
}

func TestMigratePathsVerifiesPublicURLsBeforeMetadata(t *testing.T) {
	ctx, r, s := realRepository(t)
	if _, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(batchPNG(t, 41)), Name: "one.png"}}, AddOptions{Namespace: "blog"}); err != nil {
		t.Fatal(err)
	}
	mustCommit(t, ctx, s, r)
	legacyGallery(t, ctx, r, s)
	setGitHubLocation(t, s)
	legacyHead, _ := r.Head(ctx)

	fail := true
	var checked []string
	s.VerifyURL = func(ctx context.Context, raw string, object domain.ContentObject) error {
		checked = append(checked, raw)
		if fail {
			return errors.New("public target not visible")
		}
		if object.Size <= 0 || object.Hash == (domain.ObjectHash{}) {
			return errors.New("unexpected proof object")
		}
		return nil
	}
	if _, err := s.MigratePaths(ctx, r, false, false); err == nil {
		t.Fatal("migration ignored a failed public target")
	}
	st, _ := s.Store.Load(ctx)
	if st.Head == legacyHead || st.Publication != nil || len(st.Assets) != 1 {
		t.Fatalf("failed URL verification left an unusable state: %#v", st)
	}
	for _, asset := range st.Assets {
		if asset.Path != "" {
			t.Fatalf("metadata switched before URL verification: %#v", asset)
		}
	}
	if len(checked) == 0 {
		t.Fatal("no public URL was checked")
	}

	fail = false
	result, err := s.MigratePaths(ctx, r, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Migrated != 1 {
		t.Fatalf("retry = %#v", result)
	}
	snap, err := repository.ReadSnapshot(ctx, r, result.Head)
	if err != nil {
		t.Fatal(err)
	}
	for _, asset := range snap.Assets {
		want := repository.AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext)
		if asset.Path != want {
			t.Fatalf("metadata did not switch after verification: %#v", asset)
		}
		if _, err := findEntry(ctx, r, repository.LegacyObjectPath(asset.Namespace, asset.Object)); err != nil {
			t.Fatalf("old object was removed before prune: %v", err)
		}
	}
}

func TestMigratePathsDuplicateLegacyPathsAndPruneIsExact(t *testing.T) {
	ctx, r, s := realRepository(t)
	data := batchPNG(t, 51)
	if _, err := s.Add(ctx, []Input{
		{Reader: bytes.NewReader(data), Name: "a.png"},
		{Reader: bytes.NewReader(data), Name: "b.png"},
	}, AddOptions{Namespace: "blog"}); err != nil {
		t.Fatal(err)
	}
	mustCommit(t, ctx, s, r)
	legacyGallery(t, ctx, r, s)
	legacy := snapshotAt(t, ctx, r)
	var oldPath string
	for _, asset := range legacy.Assets {
		p := repository.LegacyObjectPath(asset.Namespace, asset.Object)
		if oldPath == "" {
			oldPath = p
		} else if oldPath != p {
			t.Fatal("fixture did not share the legacy path")
		}
	}
	unrelated := blob(t, ctx, r, []byte("unrelated production object"))
	unrelatedPath := "objects/blog/sha256/ff/ee/not-managed-by-any-asset.bin"
	publishRaw(t, ctx, r, []repository.TreeChange{{Path: unrelatedPath, Blob: unrelated}})
	if _, err := s.Sync(ctx, r); err != nil {
		t.Fatal(err)
	}

	if _, err := s.MigratePaths(ctx, r, false, true); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("prune in the migration pass was not refused: %v", err)
	}
	result, err := s.MigratePaths(ctx, r, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Migrated != 2 {
		t.Fatalf("migration = %#v", result)
	}
	snap, err := repository.ReadSnapshot(ctx, r, result.Head)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	var objects []domain.ContentObject
	for _, asset := range snap.Assets {
		paths = append(paths, repository.AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext))
		objects = append(objects, asset.Object)
	}
	if len(paths) != 2 || paths[0] == paths[1] || objects[0] != objects[1] {
		t.Fatalf("duplicate assets were not split by asset id: %#v %#v", paths, objects)
	}
	if _, err := findEntry(ctx, r, oldPath); err != nil {
		t.Fatalf("shared legacy object was removed before prune: %v", err)
	}

	pruned, err := s.MigratePaths(ctx, r, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if pruned.Pruned != 1 {
		t.Fatalf("shared old path should be deleted once: %#v", pruned)
	}
	if _, err := findEntry(ctx, r, oldPath); err == nil {
		t.Fatal("mapped legacy object survived prune")
	}
	if _, err := findEntry(ctx, r, unrelatedPath); err != nil {
		t.Fatalf("unrelated objects/ object was deleted: %v", err)
	}
	for _, path := range paths {
		if _, err := findEntry(ctx, r, path); err != nil {
			t.Fatalf("live object %s was deleted: %v", path, err)
		}
	}
	// The committed retention marker is what protects mirror keys on other
	// devices after Git no longer has the old blobs.
	after := snapshotAt(t, ctx, r)
	if _, err := findEntry(ctx, r, migrationRetentionPath); err != nil {
		t.Fatalf("retention marker missing: %v", err)
	}
	keys, err := snapshotRetentionKeys(ctx, r, after, after.Manifest.ID)
	if err != nil || !keys[oldPath] {
		t.Fatalf("retention marker does not contain the old key: %v %#v", err, keys)
	}
}

func TestMigratePathsCustomOldPathUsesDurableMapping(t *testing.T) {
	ctx, r, s := realRepository(t)
	view := stageImage(t, ctx, s)
	mustCommit(t, ctx, s, r)
	head, _ := r.Head(ctx)
	snap, err := repository.ReadSnapshot(ctx, r, head)
	if err != nil {
		t.Fatal(err)
	}
	asset := snap.Assets[view.ID]
	oldPath := repository.ObjectPath(asset)
	oldEntry, err := findEntry(ctx, r, oldPath)
	if err != nil {
		t.Fatal(err)
	}
	custom := "images/custom/old-name.png"
	updated := asset
	updated.Path = custom
	metadata, err := repository.EncodeAsset(updated)
	if err != nil {
		t.Fatal(err)
	}
	metaBlob, err := r.CreateBlob(ctx, bytes.NewReader(metadata), int64(len(metadata)))
	if err != nil {
		t.Fatal(err)
	}
	oldBlob := repository.BlobID(oldEntry.SHA)
	publishRaw(t, ctx, r, []repository.TreeChange{
		{Path: custom, Blob: &oldBlob},
		{Path: repository.AssetPath(asset.ID), Blob: &metaBlob},
	})
	if _, err := s.Sync(ctx, r); err != nil {
		t.Fatal(err)
	}
	plan, found, err := s.readMigrationPlan(ctx)
	if found {
		t.Fatalf("unexpected plan before migration: %#v", plan)
	}
	if err != nil {
		t.Fatal(err)
	}

	result, err := s.MigratePaths(ctx, r, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Migrated != 1 {
		t.Fatalf("migration = %#v", result)
	}
	plan, found, err = s.readMigrationPlan(ctx)
	if err != nil || !found || len(plan.Items) != 1 || plan.Items[0].OldPath != custom {
		t.Fatalf("durable mapping = %#v, %v, %v", plan, found, err)
	}
	pruned, err := s.MigratePaths(ctx, r, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if pruned.Pruned != 1 {
		t.Fatalf("prune = %#v", pruned)
	}
	if _, err := findEntry(ctx, r, custom); err == nil {
		t.Fatal("custom old path was not pruned from the exact mapping")
	}
	if _, err := findEntry(ctx, r, oldPath); err != nil {
		t.Fatalf("unmapped original path was deleted: %v", err)
	}
	final, err := repository.ReadSnapshot(ctx, r, pruned.Head)
	if err != nil {
		t.Fatal(err)
	}
	if final.Assets[asset.ID].Path != repository.AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext) {
		t.Fatalf("asset path = %#v", final.Assets[asset.ID])
	}
}

func TestMigratePathsCloudflareUploadsBeforeSwitchAndRetainsOldKeys(t *testing.T) {
	ctx, r, s := realRepository(t)
	fake := newFakeMirror()
	enableTestCloudflare(t, s, fake, testCloudflareConfig())
	var verified []string
	s.VerifyURL = func(ctx context.Context, raw string, object domain.ContentObject) error {
		verified = append(verified, raw)
		return nil
	}
	if _, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(batchPNG(t, 61)), Name: "one.png"}}, AddOptions{Namespace: "blog"}); err != nil {
		t.Fatal(err)
	}
	mustCommit(t, ctx, s, r)
	legacyGallery(t, ctx, r, s)
	legacy := snapshotAt(t, ctx, r)
	var oldPath, newPath string
	var asset domain.Asset
	for _, a := range legacy.Assets {
		asset = a
		oldPath = repository.LegacyObjectPath(a.Namespace, a.Object)
		newPath = repository.AssetObjectPath(a.Namespace, a.ID, a.Object.Ext)
	}

	result, err := s.MigratePaths(ctx, r, false, false)
	if err != nil {
		t.Fatalf("nested lock or R2 upload failed: %v", err)
	}
	if result.Cloudflare == nil || result.Migrated != 1 {
		t.Fatalf("migration = %#v", result)
	}
	if _, ok := fake.object(newPath); !ok {
		t.Fatal("new R2 key was not uploaded before metadata switched")
	}
	if _, ok := fake.object(oldPath); !ok {
		t.Fatal("old R2 key was never mirrored in the fixture")
	}
	if len(verified) == 0 {
		t.Fatal("no public target was verified")
	}

	// A later normal reconciliation on any device must retain the old key while
	// the Git blob still exists.
	if _, err := s.CloudflareSync(ctx, r, CloudflareOptions{}); err != nil {
		t.Fatal(err)
	}
	if keys := fake.deleteKeys(); len(keys) != 0 {
		t.Fatalf("normal sync deleted old keys: %#v", keys)
	}
	if _, ok := fake.object(oldPath); !ok {
		t.Fatal("old R2 key disappeared after normal sync")
	}

	pruned, err := s.MigratePaths(ctx, r, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if pruned.Pruned != 1 || pruned.R2Retained != 1 {
		t.Fatalf("prune = %#v", pruned)
	}
	if _, err := s.CloudflareSync(ctx, r, CloudflareOptions{}); err != nil {
		t.Fatal(err)
	}
	if keys := fake.deleteKeys(); len(keys) != 0 {
		t.Fatalf("R2 keys were deleted despite the committed retention marker: %#v", keys)
	}
	if _, ok := fake.object(oldPath); !ok {
		t.Fatal("old R2 key disappeared after prune")
	}
	_ = asset
}

func TestMigratePathsResumeUsesDurableMapping(t *testing.T) {
	ctx, r, s := realRepository(t)
	if _, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(batchPNG(t, 71)), Name: "one.png"}}, AddOptions{Namespace: "blog"}); err != nil {
		t.Fatal(err)
	}
	mustCommit(t, ctx, s, r)
	legacyGallery(t, ctx, r, s)
	legacy := snapshotAt(t, ctx, r)
	var oldPath, newPath, hash string
	for _, asset := range legacy.Assets {
		oldPath = repository.LegacyObjectPath(asset.Namespace, asset.Object)
		newPath = repository.AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext)
		hash = asset.Object.Hash.String()
	}
	first, err := s.MigratePaths(ctx, r, false, false)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the metadata commit but before the CLI wrote the
	// URL mapping: the durable plan must still reconstruct the old row.
	resumed, err := s.MigratePaths(ctx, r, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Head != first.Head || resumed.Migrated != 0 || len(resumed.Items) != 1 {
		t.Fatalf("resume = %#v", resumed)
	}
	item := resumed.Items[0]
	if item.OldPath != oldPath || item.NewPath != newPath || item.Status != "migrated" || item.SHA256 != hash || item.SizeBytes == 0 {
		t.Fatalf("resumed mapping row = %#v", item)
	}
	dir := filepath.Join(t.TempDir(), "migration")
	if _, jsonPath, err := WriteURLMapping(ctx, dir, resumed.Items); err != nil {
		t.Fatal(err)
	} else {
		raw, readErr := os.ReadFile(jsonPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		var decoded []MigrateItem
		if err := json.Unmarshal(raw, &decoded); err != nil || len(decoded) != 1 || decoded[0].OldPath != oldPath || decoded[0].Status != "migrated" {
			t.Fatalf("resumed mapping = %v %#v", err, decoded)
		}
	}
}

func TestMigrationProtectsAlreadyMigratedV2(t *testing.T) {
	ctx, r, s := realRepository(t)
	stageImage(t, ctx, s)
	mustCommit(t, ctx, s, r)
	legacyGallery(t, ctx, r, s)
	if _, err := s.MigratePaths(ctx, r, false, false); err != nil {
		t.Fatal(err)
	}
	snap := snapshotAt(t, ctx, r)
	manifest := snap.Manifest
	manifest.FormatVersion = 2
	data, err := repository.EncodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestBlob := blob(t, ctx, r, data)
	publishRaw(t, ctx, r, []repository.TreeChange{{Path: repository.ManifestPath, Blob: manifestBlob}})
	if _, err := s.Sync(ctx, r); err != nil {
		t.Fatal(err)
	}
	result, err := s.MigratePaths(ctx, r, false, false)
	if err != nil {
		t.Fatal(err)
	}
	protected := snapshotAt(t, ctx, r)
	if protected.Manifest.FormatVersion != 3 || result.Head == snap.Head || len(result.Items) != 1 || result.Items[0].Status != "migrated" {
		t.Fatalf("legacy-client barrier not installed on resume: manifest=%+v result=%+v", protected.Manifest, result)
	}
	view := stageImage(t, ctx, s)
	mustCommit(t, ctx, s, r)
	if _, err := s.Verify(ctx, r); err != nil {
		t.Fatalf("v3 gallery cannot publish or verify new assets: %v", err)
	}
	if err := s.Remove(ctx, []string{view.ID.String()}); err != nil {
		t.Fatal(err)
	}
	mustCommit(t, ctx, s, r)
	if snapshotAt(t, ctx, r).Manifest.FormatVersion != 3 {
		t.Fatal("normal commit downgraded the safety format")
	}
}

func TestMigratePathsPruneVerifiesNewTargets(t *testing.T) {
	ctx, r, s := realRepository(t)
	if _, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(batchPNG(t, 72)), Name: "one.png"}}, AddOptions{Namespace: "blog"}); err != nil {
		t.Fatal(err)
	}
	mustCommit(t, ctx, s, r)
	legacyGallery(t, ctx, r, s)
	setGitHubLocation(t, s)
	legacy := snapshotAt(t, ctx, r)
	var oldPath string
	for _, asset := range legacy.Assets {
		oldPath = repository.LegacyObjectPath(asset.Namespace, asset.Object)
	}
	fail := false
	s.VerifyURL = func(ctx context.Context, raw string, object domain.ContentObject) error {
		if fail {
			return errors.New("new target is not readable")
		}
		return nil
	}
	if _, err := s.MigratePaths(ctx, r, false, false); err != nil {
		t.Fatal(err)
	}
	fail = true
	pruned, err := s.MigratePaths(ctx, r, false, true)
	if err == nil {
		t.Fatal("prune ignored a missing public target")
	}
	if pruned.Pruned != 0 {
		t.Fatalf("failed prune reported deletions: %#v", pruned)
	}
	if _, err := findEntry(ctx, r, oldPath); err != nil {
		t.Fatalf("failed prune removed the old object: %v", err)
	}
	fail = false
	pruned, err = s.MigratePaths(ctx, r, false, true)
	if err != nil || pruned.Pruned != 1 {
		t.Fatalf("prune = %#v, %v", pruned, err)
	}
	if _, err := findEntry(ctx, r, oldPath); err == nil {
		t.Fatal("prune did not remove the mapped old object")
	}
}

func TestWriteURLMapping(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "migration")
	items := []MigrateItem{
		{AssetID: "a", OldPath: "objects/x/sha256/aa/bb/h.png", NewPath: "objects/x/a.png", OldGitHub: "u1", NewGitHub: "u2", OldR2: "r1", NewR2: "r2", SHA256: "abc", SizeBytes: 12, Status: "migrated"},
		{AssetID: "b", OldPath: "objects/x/b.png", NewPath: "objects/x/b.png", Status: "unchanged"},
	}
	csvPath, jsonPath, err := WriteURLMapping(context.Background(), dir, items)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(csvPath)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
	if err != nil || len(rows) != 3 {
		t.Fatalf("csv rows = %d: %v", len(rows), err)
	}
	if rows[0][0] != "asset_id" || rows[1][8] != "migrated" || rows[1][7] != "12" || rows[2][8] != "unchanged" || rows[0][10] != "sha256" {
		t.Fatalf("csv content: %#v", rows)
	}
	raw, err := os.ReadFile(jsonPath)
	if err != nil || !bytes.Contains(raw, []byte(`"migrated"`)) {
		t.Fatalf("json mapping: %v %s", err, raw)
	}
	var decoded []MigrateItem
	if err := json.Unmarshal(raw, &decoded); err != nil || len(decoded) != 2 {
		t.Fatalf("json mapping: %v %#v", err, decoded)
	}
}
