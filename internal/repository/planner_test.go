package repository

import (
	"errors"
	"slices"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/domain"
)

func planBase(t *testing.T, assets ...domain.Asset) Snapshot {
	t.Helper()
	id, _ := domain.NewRepositoryID()
	s := Snapshot{Manifest: domain.Repository{ID: id, FormatVersion: 1, HashAlgorithm: "sha256"}, Head: "base", Tree: "tree", Assets: make(map[domain.AssetID]domain.Asset)}
	paths := map[string]bool{}
	for _, a := range assets {
		s.Assets[a.ID] = a
		p := ObjectPath(a)
		if a.Path != "" {
			s.Manifest.FormatVersion = 2
		}
		if !paths[p] {
			s.Entries = append(s.Entries, GitEntry{Path: p, Type: "blob", Mode: "100644", SHA: "blob", Size: a.Object.Size})
			paths[p] = true
		}
	}
	return s
}
func cloneIdentity(t *testing.T, a domain.Asset) domain.Asset {
	t.Helper()
	a.ID, _ = domain.NewAssetID()
	return a
}
func mustPlan(t *testing.T, s Snapshot, changes ...domain.Change) CommitPlan {
	t.Helper()
	p, e := PlanCommit(s, domain.ChangeSet{BaseHead: s.Head, Changes: changes})
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func TestPlanDeleteSharedObject(t *testing.T) {
	a := sampleAsset(t)
	b := cloneIdentity(t, a)
	s := planBase(t, a, b)
	p := mustPlan(t, s, domain.Change{Kind: domain.Delete, AssetID: a.ID})
	if len(p.FinalAssets) != 1 || p.FinalAssets[b.ID].ID != b.ID || slices.Contains(p.Deletions, LegacyObjectPath(a.Namespace, a.Object)) || !slices.Contains(p.Deletions, AssetPath(a.ID)) {
		t.Fatal(p)
	}
	if len(s.Assets) != 2 {
		t.Fatal("base mutated")
	}
}
func TestPlanDeleteLastReference(t *testing.T) {
	a := sampleAsset(t)
	s := planBase(t, a)
	p := mustPlan(t, s, domain.Change{Kind: domain.Delete, AssetID: a.ID})
	if len(p.FinalAssets) != 0 || !slices.Contains(p.Deletions, LegacyObjectPath(a.Namespace, a.Object)) {
		t.Fatal(p)
	}
}
func TestPlanDeleteAndAddSameBytes(t *testing.T) {
	a := sampleAsset(t)
	b := cloneIdentity(t, a)
	s := planBase(t, a)
	p := mustPlan(t, s, domain.Change{Kind: domain.Delete, AssetID: a.ID}, domain.Change{Kind: domain.Add, AssetID: b.ID, Asset: &b})
	if len(p.FinalAssets) != 1 || p.FinalAssets[b.ID].ID != b.ID || len(p.NewObjects) != 0 || len(p.ReusedObjects) != 1 || slices.Contains(p.Deletions, LegacyObjectPath(a.Namespace, a.Object)) {
		t.Fatal(p)
	}
}
func TestPlanNamespaceReferenceIsolation(t *testing.T) {
	a := sampleAsset(t)
	b := cloneIdentity(t, a)
	b.Namespace = "notes"
	s := planBase(t, a, b)
	p := mustPlan(t, s, domain.Change{Kind: domain.Delete, AssetID: a.ID})
	if !slices.Contains(p.Deletions, LegacyObjectPath(a.Namespace, a.Object)) || slices.Contains(p.Deletions, LegacyObjectPath(b.Namespace, b.Object)) {
		t.Fatal(p)
	}
}
func TestPlanRejectsInvalidAndStaleChanges(t *testing.T) {
	a := sampleAsset(t)
	s := planBase(t, a)
	missing := cloneIdentity(t, a)
	cases := []struct {
		name string
		set  domain.ChangeSet
		want error
	}{{"stale", domain.ChangeSet{BaseHead: "old"}, domain.ErrConflict}, {"duplicate add", domain.ChangeSet{BaseHead: s.Head, Changes: []domain.Change{{Kind: domain.Add, AssetID: a.ID, Asset: &a}}}, domain.ErrConflict}, {"missing delete", domain.ChangeSet{BaseHead: s.Head, Changes: []domain.Change{{Kind: domain.Delete, AssetID: missing.ID}}}, domain.ErrAssetNotFound}, {"missing update", domain.ChangeSet{BaseHead: s.Head, Changes: []domain.Change{{Kind: domain.Update, AssetID: missing.ID, Asset: &missing}}}, domain.ErrAssetNotFound}, {"delete then edit", domain.ChangeSet{BaseHead: s.Head, Changes: []domain.Change{{Kind: domain.Delete, AssetID: a.ID}, {Kind: domain.Update, AssetID: a.ID, Asset: &a}}}, domain.ErrConflict}, {"missing metadata", domain.ChangeSet{BaseHead: s.Head, Changes: []domain.Change{{Kind: domain.Update, AssetID: a.ID}}}, domain.ErrConflict}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PlanCommit(s, tc.set)
			if !errors.Is(err, tc.want) {
				t.Fatal(err)
			}
		})
	}
}
func TestPlanMetadataEditAndEmptyBatch(t *testing.T) {
	a := sampleAsset(t)
	b := cloneIdentity(t, a)
	s := planBase(t, a, b)
	p := mustPlan(t, s)
	if !p.Empty() {
		t.Fatal(p)
	}
	same := mustPlan(t, s, domain.Change{Kind: domain.Update, AssetID: a.ID, Asset: &a})
	if !same.Empty() {
		t.Fatal("unchanged metadata produced writes")
	}
	b.Title = "edited"
	p = mustPlan(t, s, domain.Change{Kind: domain.Delete, AssetID: a.ID}, domain.Change{Kind: domain.Update, AssetID: b.ID, Asset: &b})
	if len(p.MetadataWrites) != 1 || p.FinalAssets[b.ID].Title != "edited" || len(p.FinalAssets) != 1 || slices.Contains(p.Deletions, LegacyObjectPath(a.Namespace, a.Object)) {
		t.Fatal(p)
	}
}

func TestPlanCustomPathReferenceCountsAcrossNamespaces(t *testing.T) {
	a := sampleAsset(t)
	a.Path = "img/shared.png"
	b := cloneIdentity(t, a)
	b.Namespace = "notes"
	c := cloneIdentity(t, a)
	c.Path = ""
	s := planBase(t, a, b, c)
	s.Entries = append(s.Entries, GitEntry{Path: "img/unmanaged.png", Type: "blob", Mode: "100644", SHA: "unmanaged", Size: 99})
	one := mustPlan(t, s, domain.Change{Kind: domain.Delete, AssetID: a.ID})
	if slices.Contains(one.Deletions, a.Path) {
		t.Fatal("removed custom path still referenced by another namespace")
	}
	both := mustPlan(t, s, domain.Change{Kind: domain.Delete, AssetID: a.ID}, domain.Change{Kind: domain.Delete, AssetID: b.ID})
	if !slices.Contains(both.Deletions, a.Path) || slices.Contains(both.Deletions, ObjectPath(c)) || slices.Contains(both.Deletions, "img/unmanaged.png") {
		t.Fatalf("full path reference isolation failed: %#v", both.Deletions)
	}
}

func TestPlanFirstExplicitPathUpgradesManifestInSameBatch(t *testing.T) {
	a := sampleAsset(t)
	a.Path = "img/existing"
	base := planBase(t)
	base.Entries = append(base.Entries, GitEntry{Path: a.Path, Type: "blob", Mode: "100644", SHA: "legacy", Size: a.Object.Size})
	plan := mustPlan(t, base, domain.Change{Kind: domain.Add, AssetID: a.ID, Asset: &a})
	manifestWrites := 0
	for _, write := range plan.MetadataWrites {
		if write.Path == ManifestPath {
			manifest, err := DecodeManifest(write.Data)
			if err != nil || manifest.FormatVersion != 2 || manifest.ID != base.Manifest.ID {
				t.Fatalf("bad upgrade: %#v, %v", manifest, err)
			}
			manifestWrites++
		}
	}
	if manifestWrites != 1 || len(plan.MetadataWrites) != 2 || len(plan.NewObjects) != 0 || len(plan.ReusedObjects) != 1 || base.Manifest.FormatVersion != 1 {
		t.Fatalf("upgrade is not atomic metadata-only adoption: %#v", plan)
	}
	a.Path = ""
	canonical := mustPlan(t, base, domain.Change{Kind: domain.Add, AssetID: a.ID, Asset: &a})
	for _, write := range canonical.MetadataWrites {
		if write.Path == ManifestPath {
			t.Fatal("ordinary canonical upload rewrites v1 manifest")
		}
	}
}
