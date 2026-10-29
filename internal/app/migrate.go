package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/atomicfile"
	"github.com/Eurekaimer/TOUKO/internal/delivery"
	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/index"
	"github.com/Eurekaimer/TOUKO/internal/r2"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

// Publication journal operations owned by migration. Ordinary commits leave
// Operation empty; recovery branches on these values instead of rebuilding a
// staged batch that migration never creates.
const (
	migrationObjectsOperation  = "migrate_objects"
	migrationMetadataOperation = "migrate_metadata"
	migrationPruneOperation    = "prune_objects"

	migrationPlanVersion = 1
)

// MigrateItem records one asset's old and new location plus the public URLs the
// operator has to rewrite. It is written to migration/url-mapping.{csv,json}.
type MigrateItem struct {
	AssetID   string `json:"asset_id"`
	OldPath   string `json:"old_path"`
	NewPath   string `json:"new_path"`
	OldGitHub string `json:"old_github_url,omitempty"`
	NewGitHub string `json:"new_github_url,omitempty"`
	OldR2     string `json:"old_r2_url,omitempty"`
	NewR2     string `json:"new_r2_url,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	SizeBytes int64  `json:"size_bytes"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
}

type MigrateResult struct {
	Head       domain.RepositoryHead `json:"head"`
	DryRun     bool                  `json:"dry_run"`
	Prune      bool                  `json:"prune"`
	Migrated   int                   `json:"migrated"`
	Unchanged  int                   `json:"unchanged"`
	Failed     int                   `json:"failed"`
	Pruned     int                   `json:"pruned"`
	R2Retained int                   `json:"r2_retained,omitempty"`
	Items      []MigrateItem         `json:"items"`
	Cloudflare *CloudflareResult     `json:"cloudflare,omitempty"`
}

// migrationPlan is the durable exact old→new mapping. Prune and every later R2
// reconciliation consume it, so custom old paths and shared legacy objects are
// handled without guessing from current metadata.
type migrationPlan struct {
	Version      int                 `json:"version"`
	RepositoryID string              `json:"repository_id"`
	Items        []migrationPlanItem `json:"items"`
}

type migrationPlanItem struct {
	AssetID   string               `json:"asset_id"`
	Namespace domain.Namespace     `json:"namespace"`
	OldPath   string               `json:"old_path"`
	NewPath   string               `json:"new_path"`
	Object    domain.ContentObject `json:"object"`
	// SHA256 and Blob preserve the exact source proof across recovery.
	SHA256 string `json:"sha256"`
	Blob   string `json:"blob"`
}

type migrationProof struct {
	SHA256 string
	Blob   string
}

// migrationAnalysis is the read-only view of what a migration run would do.
type migrationAnalysis struct {
	items       []MigrateItem
	planItems   []migrationPlanItem
	assets      []domain.Asset
	oldEntries  map[domain.AssetID]repository.GitEntry
	changed     map[domain.AssetID]bool
	missing     map[domain.AssetID]bool
	needObjects bool
	needMeta    bool
}

// MigratePaths rewrites every asset to objects/<namespace>/<asset-id>.<ext> in
// two published phases. Phase one adds the new object entries while metadata
// still points at the old paths; phase two switches metadata in one commit.
// Old objects and old URLs therefore survive every failure, and only prune can
// remove the exact entries recorded in the durable migration plan.
func (s *Service) MigratePaths(ctx context.Context, r repository.Git, dryRun, prune bool) (MigrateResult, error) {
	unlock, err := s.Lock(ctx)
	if err != nil {
		return MigrateResult{}, err
	}
	defer unlock()
	st, err := s.Store.Load(ctx)
	if err != nil {
		return MigrateResult{}, err
	}
	if err = mutable(st); err != nil {
		return MigrateResult{}, err
	}
	// Migration is not a user batch. Refuse instead of replacing or clearing
	// staged work that a later commit still owns.
	if len(st.Changes) > 0 {
		return MigrateResult{}, &domain.ConflictError{Base: st.BaseHead, Remote: st.Head, Reason: "staged_changes"}
	}
	head, err := r.Head(ctx)
	if err != nil {
		return MigrateResult{}, err
	}
	if head != st.Head {
		return MigrateResult{}, &domain.ConflictError{Base: st.Head, Remote: head, Reason: "head_changed"}
	}
	base, err := repository.ReadSnapshot(ctx, r, head)
	if err != nil {
		return MigrateResult{}, err
	}
	if err = sameRepository(st, base); err != nil {
		return MigrateResult{}, err
	}
	location, locationErr := s.Location(ctx)
	if locationErr != nil {
		if _, statErr := os.Stat(filepath.Join(s.Dir, "config.json")); !errors.Is(statErr, os.ErrNotExist) {
			return MigrateResult{}, locationErr
		}
	}
	cfg, err := s.CloudflareConfig(ctx)
	if err != nil {
		return MigrateResult{}, err
	}
	helpers := urlBuilder{location: location, r2Base: cfg.PublicBaseURL}
	analysis, err := s.analyzeMigration(ctx, r, base, helpers)
	if err != nil {
		return MigrateResult{}, err
	}
	result := MigrateResult{Head: head, DryRun: dryRun, Prune: prune, Items: analysis.items}
	if prune {
		// Prune is a separate, explicitly destructive step. Never mix it with a
		// metadata switch that has not been verified yet.
		if analysis.needObjects || analysis.needMeta {
			return result, fmt.Errorf("%w: prune requires every asset to use the asset-id layout; run migrate paths first", domain.ErrConflict)
		}
		return s.pruneObjects(ctx, r, st, base, result, dryRun)
	}
	if dryRun {
		for i := range result.Items {
			if result.Items[i].Status == "planned" {
				result.Items[i].Status = "would_migrate"
				result.Migrated++
			} else {
				result.Unchanged++
			}
		}
		return result, nil
	}
	if analysis.needMeta || analysis.needObjects {
		merged, err := s.mergeMigrationPlan(ctx, st.Manifest.ID, analysis.planItems)
		if err != nil {
			return result, err
		}
		if err = s.writeMigrationPlan(ctx, merged); err != nil {
			return result, err
		}
	}
	if analysis.needObjects {
		base, err = s.publishMigrationObjects(ctx, r, st, base, analysis)
		if err != nil {
			return result, err
		}
		head = base.Head
		result.Head = head
		// Re-derive the plan from the authoritative snapshot; a concurrent
		// descendant must not be silently mixed into the metadata switch.
		analysis, err = s.analyzeMigration(ctx, r, base, helpers)
		if err != nil {
			return result, err
		}
		if analysis.needObjects {
			return result, &domain.ConflictError{Base: head, Remote: head, Reason: "head_changed"}
		}
		result.Items = analysis.items
	}
	if analysis.needMeta {
		// New GitHub objects are visible after phase one. Do not expose metadata
		// until the exact public targets and the R2 mirror can be read back.
		targets := analysis.changedAssets()
		if base.Manifest.FormatVersion != 3 {
			targets = analysis.assets
		}
		mirror, verifyErr := s.verifyMigrationTargets(ctx, r, base, targets, location, cfg)
		result.Cloudflare = mirror
		if verifyErr != nil {
			return result, verifyErr
		}
		base, err = s.publishMigrationMetadata(ctx, r, st, base, analysis)
		if err != nil {
			return result, err
		}
		head = base.Head
		result.Head = head
	}
	published, err := repository.ReadSnapshot(ctx, r, result.Head)
	if err != nil {
		return result, err
	}
	if err = s.verifyPublishedTargets(ctx, r, published, analysis); err != nil {
		return result, err
	}
	result.Items = analysis.items
	for i := range result.Items {
		if result.Items[i].Status != "planned" {
			result.Unchanged++
			continue
		}
		result.Items[i].Status = "migrated"
		result.Migrated++
	}
	// A rerun with no metadata work still re-checks published targets: GitHub
	// bytes when the backend has public URLs and, when enabled, a partial R2
	// mirror that an earlier failure left behind.
	if !analysis.needMeta && !analysis.needObjects && (cfg.Enabled || location.Backend == "github") {
		mirror, verifyErr := s.verifyMigrationTargets(ctx, r, published, analysis.assets, location, cfg)
		result.Cloudflare = mirror
		if verifyErr != nil {
			return result, verifyErr
		}
	}
	return result, nil
}

func (a migrationAnalysis) changedAssets() []domain.Asset {
	out := make([]domain.Asset, 0, len(a.changed))
	for _, asset := range a.assets {
		if a.changed[asset.ID] {
			out = append(out, asset)
		}
	}
	return out
}

func (s *Service) analyzeMigration(ctx context.Context, r repository.Reader, base repository.Snapshot, helpers urlBuilder) (migrationAnalysis, error) {
	a := migrationAnalysis{
		items:      []MigrateItem{},
		planItems:  []migrationPlanItem{},
		oldEntries: make(map[domain.AssetID]repository.GitEntry),
		changed:    make(map[domain.AssetID]bool),
		missing:    make(map[domain.AssetID]bool),
	}
	plan, found, err := s.readMigrationPlan(ctx)
	if err != nil {
		return a, err
	}
	planByID := make(map[domain.AssetID]migrationPlanItem)
	if found {
		if plan.RepositoryID != base.Manifest.ID.String() {
			return a, fmt.Errorf("%w: migration mapping belongs to another repository", domain.ErrConflict)
		}
		for _, item := range plan.Items {
			id, err := domain.ParseAssetID(item.AssetID)
			if err != nil {
				return a, fmt.Errorf("%w: invalid migration mapping asset id", domain.ErrRepositoryCorrupt)
			}
			planByID[id] = item
		}
	}
	ids := make([]domain.AssetID, 0, len(base.Assets))
	for id := range base.Assets {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	targets := make(map[string]domain.AssetID, len(ids))
	for _, id := range ids {
		asset := base.Assets[id]
		if asset.Object.Hash == (domain.ObjectHash{}) {
			return a, fmt.Errorf("%w: asset %s has no SHA-256", domain.ErrRepositoryCorrupt, id)
		}
		a.assets = append(a.assets, asset)
		newPath := repository.AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext)
		if other, ok := targets[newPath]; ok && other != id {
			return a, fmt.Errorf("%w: two assets share target path %s", domain.ErrRepositoryCorrupt, newPath)
		}
		targets[newPath] = id
		oldPath := repository.ObjectPath(asset)
		// The durable plan is the only exact record of the old path once a
		// resumed run sees metadata already pointing at the new location.
		mappedOld := oldPath
		planned, hasPlan := planByID[id]
		if hasPlan && planned.NewPath == newPath && planned.Namespace == asset.Namespace && planned.Object == asset.Object {
			if oldPath == newPath {
				mappedOld = planned.OldPath
			} else if planned.OldPath != oldPath {
				return a, fmt.Errorf("%w: migration mapping records old path %s for asset %s, but metadata uses %s", domain.ErrConflict, planned.OldPath, id, oldPath)
			}
		}
		item := MigrateItem{
			AssetID: asset.ID.String(), OldPath: mappedOld, NewPath: newPath,
			SizeBytes: asset.Object.Size, Status: "unchanged", SHA256: asset.Object.Hash.String(),
		}
		item.OldGitHub, item.NewGitHub = helpers.github(mappedOld), helpers.github(newPath)
		item.OldR2, item.NewR2 = helpers.r2(mappedOld), helpers.r2(newPath)
		if oldPath == newPath {
			if mappedOld != oldPath {
				// Rebuild the exact old mapping from the durable plan.
				item.Status = "migrated"
				if base.Manifest.FormatVersion != 3 {
					a.needMeta = true
				}
			}
			a.items = append(a.items, item)
			continue
		}
		oldEntry, ok := entryAt(base.Entries, oldPath)
		if !ok {
			return a, fmt.Errorf("%w: base object %s is missing", domain.ErrRepositoryCorrupt, oldPath)
		}
		if err := checkBlobEntry(oldEntry, asset.Object.Size); err != nil {
			return a, err
		}
		proof, err := sourceProof(ctx, r, oldEntry, asset.Object)
		if err != nil {
			return a, err
		}
		item.SHA256 = proof.SHA256
		a.oldEntries[id] = oldEntry
		a.changed[id] = true
		a.planItems = append(a.planItems, migrationPlanItem{
			AssetID: id.String(), Namespace: asset.Namespace, OldPath: oldPath, NewPath: newPath, Object: asset.Object,
			SHA256: proof.SHA256, Blob: proof.Blob,
		})
		if target, ok := entryAt(base.Entries, newPath); ok {
			// A resumed phase one must reuse the exact same immutable blob.
			// Same-size wrong bytes are never accepted as a completed target.
			if err := checkBlobEntry(target, asset.Object.Size); err != nil {
				return a, err
			}
			if target.SHA != oldEntry.SHA {
				return a, fmt.Errorf("%w: target path %s is occupied by different bytes", domain.ErrConflict, newPath)
			}
		} else {
			a.missing[id] = true
			a.needObjects = true
		}
		a.needMeta = true
		item.Status = "planned"
		a.items = append(a.items, item)
	}
	return a, nil
}

func (s *Service) publishMigrationObjects(ctx context.Context, r repository.Git, st index.State, base repository.Snapshot, a migrationAnalysis) (repository.Snapshot, error) {
	changes := make([]repository.TreeChange, 0, len(a.missing))
	for _, id := range sortedIDs(a.missing) {
		asset := base.Assets[id]
		oldEntry, ok := a.oldEntries[id]
		if !ok {
			return repository.Snapshot{}, fmt.Errorf("%w: migration source for %s is unknown", domain.ErrRepositoryCorrupt, asset.ID)
		}
		// Git blobs are immutable and content addressed: the new path can point
		// at the already verified old blob without another upload.
		blob := repository.BlobID(oldEntry.SHA)
		changes = append(changes, repository.TreeChange{
			Path: repository.AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext),
			Blob: &blob,
		})
	}
	if len(changes) == 0 {
		return base, nil
	}
	tree, err := r.CreateTree(ctx, base.Tree, changes)
	if err != nil {
		return repository.Snapshot{}, err
	}
	candidateHead, err := r.CreateCommit(ctx, tree, base.Head, "migrate: add asset-id object copies")
	if err != nil {
		return repository.Snapshot{}, err
	}
	candidate, err := repository.ReadSnapshot(ctx, r, candidateHead)
	if err != nil {
		return repository.Snapshot{}, err
	}
	if err = validateMigrationObjects(ctx, r, base, candidate); err != nil {
		return repository.Snapshot{}, err
	}
	pub := &index.Publication{
		BaseHead: base.Head, CandidateHead: candidateHead, CandidateTree: string(tree),
		Operation: migrationObjectsOperation,
	}
	st.Publication = pub
	if err = s.Store.SavePublication(ctx, pub); err != nil {
		return repository.Snapshot{}, err
	}
	published, err := s.publish(ctx, r, st, candidate)
	if err != nil {
		return repository.Snapshot{}, err
	}
	if published.Head != candidateHead {
		return repository.Snapshot{}, &domain.ConflictError{Base: candidateHead, Remote: published.Head, Reason: "head_changed"}
	}
	// Re-read the published tree; the journal is not cleared until this hold.
	return repository.ReadSnapshot(ctx, r, published.Head)
}

func (s *Service) publishMigrationMetadata(ctx context.Context, r repository.Git, st index.State, base repository.Snapshot, a migrationAnalysis) (repository.Snapshot, error) {
	writes := make([]repository.MetadataWrite, 0, len(a.changed))
	blobs := make(map[string]repository.BlobID, len(a.changed))
	for _, id := range sortedIDs(a.changed) {
		asset := base.Assets[id]
		updated := asset
		updated.Path = repository.AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext)
		data, err := repository.EncodeAsset(updated)
		if err != nil {
			return repository.Snapshot{}, err
		}
		blob, err := r.CreateBlob(ctx, bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return repository.Snapshot{}, err
		}
		blobs[repository.AssetPath(id)] = blob
		writes = append(writes, repository.MetadataWrite{Path: repository.AssetPath(id), Data: data})
	}
	// Older clients could delete legacy R2 keys after the metadata switch.
	if base.Manifest.FormatVersion != 3 {
		manifest := base.Manifest
		manifest.FormatVersion = 3
		data, err := repository.EncodeManifest(manifest)
		if err != nil {
			return repository.Snapshot{}, err
		}
		blob, err := r.CreateBlob(ctx, bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return repository.Snapshot{}, err
		}
		blobs[repository.ManifestPath] = blob
		writes = append(writes, repository.MetadataWrite{Path: repository.ManifestPath, Data: data})
	}
	if len(writes) == 0 {
		return base, nil
	}
	changes := make([]repository.TreeChange, 0, len(writes))
	for _, write := range writes {
		blob := blobs[write.Path]
		changes = append(changes, repository.TreeChange{Path: write.Path, Blob: &blob})
	}
	tree, err := r.CreateTree(ctx, base.Tree, changes)
	if err != nil {
		return repository.Snapshot{}, err
	}
	candidateHead, err := r.CreateCommit(ctx, tree, base.Head, "migrate: use asset ids in object paths")
	if err != nil {
		return repository.Snapshot{}, err
	}
	candidate, err := repository.ReadSnapshot(ctx, r, candidateHead)
	if err != nil {
		return repository.Snapshot{}, err
	}
	if err = validateMigrationMetadata(ctx, r, base, candidate); err != nil {
		return repository.Snapshot{}, err
	}
	pub := &index.Publication{
		BaseHead: base.Head, CandidateHead: candidateHead, CandidateTree: string(tree),
		Operation: migrationMetadataOperation,
	}
	st.Publication = pub
	if err = s.Store.SavePublication(ctx, pub); err != nil {
		return repository.Snapshot{}, err
	}
	published, err := s.publish(ctx, r, st, candidate)
	if err != nil {
		return repository.Snapshot{}, err
	}
	return repository.ReadSnapshot(ctx, r, published.Head)
}

// verifyMigrationTargets requires every target to be a real GitHub object at
// the published head and, when URLs exist, readable with the exact bytes at
// the public URL. R2 is reconciled upload-only first so old mirrored keys are
// never deleted by a metadata migration.
func (s *Service) verifyMigrationTargets(ctx context.Context, r repository.Git, snap repository.Snapshot, assets []domain.Asset, location repository.Location, cfg r2.Config) (*CloudflareResult, error) {
	var result *CloudflareResult
	if cfg.Enabled {
		mirror, err := s.cloudflareSyncLocked(ctx, r, CloudflareOptions{SuppressDeletes: true, ExtraAssets: assets})
		result = &mirror
		if err != nil {
			return result, err
		}
	}
	verifier := s.VerifyURL
	if verifier == nil {
		verifier = verifyPublicObject
	}
	plan, err := s.planProofItems(ctx)
	if err != nil {
		return result, err
	}
	helpers := urlBuilder{location: location, r2Base: cfg.PublicBaseURL}
	for _, asset := range assets {
		newPath := repository.AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext)
		entry, ok := entryAt(snap.Entries, newPath)
		if !ok {
			return result, fmt.Errorf("%w: new object %s is not published", domain.ErrRepositoryCorrupt, newPath)
		}
		proof, err := s.assetProof(ctx, r, snap, asset, plan)
		if err != nil {
			return result, err
		}
		if err := verifyTargetWithProof(ctx, r, entry, asset.Object, proof); err != nil {
			return result, err
		}
		if location.Backend == "github" {
			if location.Private {
				return result, fmt.Errorf("%w: public URL verification requires a public GitHub repository", domain.ErrDeliveryUnavailable)
			}
			url := helpers.github(newPath)
			if url == "" {
				return result, fmt.Errorf("%w: cannot build GitHub URL for %s", domain.ErrDeliveryUnavailable, newPath)
			}
			if err := verifier(ctx, url, asset.Object); err != nil {
				return result, err
			}
		}
		if cfg.Enabled {
			url := helpers.r2(newPath)
			if url == "" {
				return result, fmt.Errorf("%w: cannot build R2 URL for %s", domain.ErrDeliveryUnavailable, newPath)
			}
			if err := verifier(ctx, url, asset.Object); err != nil {
				return result, err
			}
		}
	}
	return result, nil
}

func (s *Service) verifyPublishedTargets(ctx context.Context, r repository.Reader, snap repository.Snapshot, a migrationAnalysis) error {
	plan, err := s.planProofItems(ctx)
	if err != nil {
		return err
	}
	for id := range a.changed {
		asset, ok := snap.Assets[id]
		if !ok {
			return fmt.Errorf("%w: migrated asset %s disappeared", domain.ErrRepositoryCorrupt, id)
		}
		newPath := repository.AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext)
		if asset.Path != newPath {
			return fmt.Errorf("%w: asset %s does not use its asset-id path", domain.ErrRepositoryCorrupt, id)
		}
		entry, ok := entryAt(snap.Entries, newPath)
		if !ok {
			return fmt.Errorf("%w: new object %s is not published", domain.ErrRepositoryCorrupt, newPath)
		}
		proof, err := s.assetProof(ctx, r, snap, asset, plan)
		if err != nil {
			return err
		}
		if err := verifyTargetWithProof(ctx, r, entry, asset.Object, proof); err != nil {
			return err
		}
	}
	return nil
}

func verifyTargetWithProof(ctx context.Context, r repository.Reader, entry repository.GitEntry, object domain.ContentObject, proof migrationProof) error {
	if err := checkBlobEntry(entry, object.Size); err != nil {
		return err
	}
	if proof.Blob != "" && entry.SHA != proof.Blob {
		return fmt.Errorf("%w: target %s is not the recorded source blob", domain.ErrRepositoryCorrupt, entry.Path)
	}
	if object.Hash == (domain.ObjectHash{}) || proof.SHA256 != object.Hash.String() {
		return fmt.Errorf("%w: recorded source proof differs from metadata", domain.ErrRepositoryCorrupt)
	}
	return repository.VerifyObject(ctx, r, repository.BlobID(entry.SHA), object)
}

func (s *Service) assetProof(ctx context.Context, r repository.Reader, snap repository.Snapshot, asset domain.Asset, plan map[domain.AssetID]migrationPlanItem) (migrationProof, error) {
	if item, ok := plan[asset.ID]; ok {
		newPath := repository.AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext)
		if item.NewPath != newPath || item.Object != asset.Object {
			return migrationProof{}, fmt.Errorf("%w: durable mapping does not match asset %s", domain.ErrRepositoryCorrupt, asset.ID)
		}
		return migrationProof{SHA256: item.SHA256, Blob: item.Blob}, nil
	}
	newPath := repository.AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext)
	entry, ok := entryAt(snap.Entries, newPath)
	if !ok {
		return migrationProof{}, fmt.Errorf("%w: new object %s is not published", domain.ErrRepositoryCorrupt, newPath)
	}
	if err := checkBlobEntry(entry, asset.Object.Size); err != nil {
		return migrationProof{}, err
	}
	if asset.Object.Hash == (domain.ObjectHash{}) {
		return migrationProof{}, fmt.Errorf("%w: asset %s has no SHA-256", domain.ErrRepositoryCorrupt, asset.ID)
	}
	return migrationProof{SHA256: asset.Object.Hash.String(), Blob: entry.SHA}, nil
}

func (s *Service) planProofItems(ctx context.Context) (map[domain.AssetID]migrationPlanItem, error) {
	plan, found, err := s.readMigrationPlan(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[domain.AssetID]migrationPlanItem)
	if !found {
		return out, nil
	}
	for _, item := range plan.Items {
		id, err := domain.ParseAssetID(item.AssetID)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid migration mapping asset id", domain.ErrRepositoryCorrupt)
		}
		out[id] = item
	}
	return out, nil
}

// pruneObjects removes only the exact old paths recorded in the durable plan,
// after every current asset uses a verified asset-id path. It never derives
// candidates from hashes and never touches metadata or unrelated objects.
func (s *Service) pruneObjects(ctx context.Context, r repository.Git, st index.State, base repository.Snapshot, result MigrateResult, dryRun bool) (MigrateResult, error) {
	plan, found, err := s.readMigrationPlan(ctx)
	if err != nil {
		return result, err
	}
	if !found {
		return result, fmt.Errorf("%w: no durable migration mapping; run migrate paths first", domain.ErrConflict)
	}
	if st.Manifest == nil || plan.RepositoryID != st.Manifest.ID.String() {
		return result, fmt.Errorf("%w: migration mapping belongs to another repository", domain.ErrConflict)
	}
	planByID := make(map[string]migrationPlanItem, len(plan.Items))
	for _, item := range plan.Items {
		planByID[item.AssetID] = item
	}
	// Every asset must already point at a verified asset-id object.
	referenced := make(map[string]bool, len(base.Assets))
	for id, asset := range base.Assets {
		newPath := repository.AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext)
		if asset.Path != newPath {
			return result, fmt.Errorf("%w: asset %s is not migrated; run migrate paths first", domain.ErrConflict, id)
		}
		entry, ok := entryAt(base.Entries, newPath)
		if !ok {
			return result, fmt.Errorf("%w: live object %s is missing", domain.ErrRepositoryCorrupt, newPath)
		}
		if item, ok := planByID[id.String()]; ok && item.NewPath == newPath && item.Object == asset.Object {
			if err := verifyMappedObject(ctx, r, entry, item); err != nil {
				return result, err
			}
		} else if err := verifyEntryObject(ctx, r, entry, asset.Object); err != nil {
			return result, err
		}
		referenced[newPath] = true
	}
	entries := make(map[string]repository.GitEntry, len(base.Entries))
	for _, entry := range base.Entries {
		entries[entry.Path] = entry
	}
	allowed := make(map[string]migrationPlanItem, len(plan.Items))
	gitDeletions := make([]string, 0, len(plan.Items))
	r2Keys := make([]string, 0, len(plan.Items))
	for _, item := range plan.Items {
		id, err := domain.ParseAssetID(item.AssetID)
		if err != nil {
			return result, fmt.Errorf("%w: invalid migration mapping asset id", domain.ErrRepositoryCorrupt)
		}
		asset, ok := base.Assets[id]
		if !ok {
			// The asset no longer exists; keep the mapping and never delete an
			// object that no current asset can vouch for.
			continue
		}
		newPath := repository.AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext)
		if asset.Path != newPath || item.Namespace != asset.Namespace || item.Object != asset.Object || item.NewPath != newPath {
			return result, fmt.Errorf("%w: migration mapping does not match asset %s", domain.ErrConflict, id)
		}
		if item.OldPath == item.NewPath {
			return result, fmt.Errorf("%w: migration mapping has an empty move for %s", domain.ErrRepositoryCorrupt, id)
		}
		if referenced[item.OldPath] {
			return result, fmt.Errorf("%w: old path %s is still live", domain.ErrConflict, item.OldPath)
		}
		if entry, ok := entries[item.OldPath]; ok {
			if err := verifyMappedObject(ctx, r, entry, item); err != nil {
				return result, err
			}
			gitDeletions = append(gitDeletions, item.OldPath)
		}
		allowed[item.OldPath] = item
		r2Keys = append(r2Keys, item.OldPath)
	}
	gitDeletions = uniqueSorted(gitDeletions)
	r2Keys = uniqueSorted(r2Keys)
	cfg, err := s.CloudflareConfig(ctx)
	if err != nil {
		return result, err
	}
	if dryRun {
		result.Pruned = len(gitDeletions)
		if cfg.Enabled {
			// R2 has no conditional delete (a mismatched If-Match was observed
			// to delete anyway), so migration never removes mirror keys. The
			// committed retention marker keeps them excluded for every device.
			result.R2Retained = len(r2Keys)
		}
		return result, nil
	}
	location, locationErr := s.Location(ctx)
	if locationErr != nil {
		if _, statErr := os.Stat(filepath.Join(s.Dir, "config.json")); !errors.Is(statErr, os.ErrNotExist) {
			return result, locationErr
		}
	}
	assets := make([]domain.Asset, 0, len(base.Assets))
	for _, asset := range base.Assets {
		assets = append(assets, asset)
	}
	mirror, err := s.verifyMigrationTargets(ctx, r, base, assets, location, cfg)
	result.Cloudflare = mirror
	if err != nil {
		return result, err
	}
	retention, err := retentionForPrune(ctx, r, base, st.Manifest.ID, plan)
	if err != nil {
		return result, err
	}
	var retentionBlob *repository.BlobID
	if len(retention) > 0 {
		existing, ok := entryAt(base.Entries, migrationRetentionPath)
		changed := true
		if ok {
			data, readErr := entryBytes(ctx, r, existing)
			if readErr != nil {
				return result, readErr
			}
			changed = !bytes.Equal(data, retention)
		}
		if changed {
			blob, err := r.CreateBlob(ctx, bytes.NewReader(retention), int64(len(retention)))
			if err != nil {
				return result, err
			}
			retentionBlob = &blob
		}
	}
	if len(gitDeletions) > 0 || retentionBlob != nil {
		changes := make([]repository.TreeChange, 0, len(gitDeletions)+1)
		for _, path := range gitDeletions {
			changes = append(changes, repository.TreeChange{Path: path})
		}
		if retentionBlob != nil {
			changes = append(changes, repository.TreeChange{Path: migrationRetentionPath, Blob: retentionBlob})
		}
		tree, err := r.CreateTree(ctx, base.Tree, changes)
		if err != nil {
			return result, err
		}
		candidateHead, err := r.CreateCommit(ctx, tree, base.Head, "migrate: remove legacy content-addressed objects")
		if err != nil {
			return result, err
		}
		candidate, err := repository.ReadSnapshot(ctx, r, candidateHead)
		if err != nil {
			return result, err
		}
		if err = validatePruneCandidate(ctx, r, base, candidate, allowed, retention); err != nil {
			return result, err
		}
		pub := &index.Publication{
			BaseHead: base.Head, CandidateHead: candidateHead, CandidateTree: string(tree),
			Operation: migrationPruneOperation, Deletions: gitDeletions,
		}
		st.Publication = pub
		if err = s.Store.SavePublication(ctx, pub); err != nil {
			return result, err
		}
		published, err := s.publish(ctx, r, st, candidate)
		if err != nil {
			return result, err
		}
		result.Head = published.Head
	}
	result.Pruned = len(gitDeletions)
	if cfg.Enabled {
		result.R2Retained = len(r2Keys)
	}
	// The durable plan intentionally survives prune: it is the only exact
	// record of old mirror keys and must keep protecting them from later
	// unconditional reconciliation.
	return result, nil
}

// ---------------------------------------------------------------------------
// Journal recovery for operation publications.

func (s *Service) publishOperationJournal(ctx context.Context, r repository.Git, st index.State, base *repository.Snapshot) (SyncResult, error) {
	if len(st.Changes) > 0 {
		return SyncResult{}, fmt.Errorf("%w: migration journal cannot coexist with staged changes", domain.ErrConflict)
	}
	candidate, err := s.operationCandidate(ctx, r, st, base)
	if err != nil {
		return SyncResult{}, err
	}
	return s.publish(ctx, r, st, candidate)
}

func (s *Service) finalizeOperationPublished(ctx context.Context, r repository.Git, st index.State, head domain.RepositoryHead) (SyncResult, error) {
	if len(st.Changes) > 0 {
		return SyncResult{}, fmt.Errorf("%w: migration journal cannot coexist with staged changes", domain.ErrConflict)
	}
	candidate, err := s.operationCandidate(ctx, r, st, nil)
	if err != nil {
		return SyncResult{}, err
	}
	snap, err := currentSnapshot(ctx, r, candidate, head)
	if err != nil {
		return SyncResult{}, err
	}
	return s.finalizeSnapshot(ctx, r, st, snap)
}

func (s *Service) operationCandidate(ctx context.Context, r repository.Reader, st index.State, trusted *repository.Snapshot) (repository.Snapshot, error) {
	p := st.Publication
	if p == nil {
		return repository.Snapshot{}, fmt.Errorf("%w: missing publication journal", domain.ErrRepositoryCorrupt)
	}
	var base repository.Snapshot
	var err error
	if trusted != nil {
		base = *trusted
	} else {
		base, err = repository.ReadSnapshot(ctx, r, p.BaseHead)
		if err != nil {
			return repository.Snapshot{}, err
		}
	}
	if base.Head != p.BaseHead {
		return repository.Snapshot{}, fmt.Errorf("%w: journal base differs", domain.ErrRepositoryCorrupt)
	}
	candidate, err := repository.ReadSnapshot(ctx, r, p.CandidateHead)
	if err != nil {
		return repository.Snapshot{}, err
	}
	commit, err := r.Commit(ctx, p.CandidateHead)
	if err != nil {
		return repository.Snapshot{}, err
	}
	if len(commit.Parents) != 1 || commit.Parents[0] != p.BaseHead || string(candidate.Tree) != p.CandidateTree {
		return repository.Snapshot{}, fmt.Errorf("%w: operation candidate has an unexpected shape", domain.ErrRepositoryCorrupt)
	}
	if err = sameRepository(st, candidate); err != nil {
		return repository.Snapshot{}, err
	}
	switch p.Operation {
	case migrationObjectsOperation, migrationMetadataOperation:
		if p.Operation == migrationObjectsOperation {
			err = validateMigrationObjects(ctx, r, base, candidate)
		} else {
			err = validateMigrationMetadata(ctx, r, base, candidate)
		}
		if err == nil {
			var plan migrationPlan
			var found bool
			plan, found, err = s.readMigrationPlan(ctx)
			if err == nil && !found {
				err = fmt.Errorf("%w: migration journal has no durable mapping", domain.ErrRepositoryCorrupt)
			}
			if err == nil {
				err = verifyMigrationOperationProofs(ctx, r, base, candidate, plan)
			}
		}
	case migrationPruneOperation:
		var plan migrationPlan
		var found bool
		plan, found, err = s.readMigrationPlan(ctx)
		if err == nil && !found {
			err = fmt.Errorf("%w: prune journal has no durable mapping", domain.ErrRepositoryCorrupt)
		}
		if err == nil {
			var retention []byte
			retention, err = retentionForPrune(ctx, r, base, base.Manifest.ID, plan)
			if err == nil {
				err = validatePruneCandidate(ctx, r, base, candidate, planObjects(base, plan), retention)
			}
		}
	default:
		err = fmt.Errorf("%w: unknown publication operation %q", domain.ErrRepositoryCorrupt, p.Operation)
	}
	if err != nil {
		return repository.Snapshot{}, err
	}
	return candidate, nil
}

// verifyMigrationOperationProofs re-proves the exact source/copy relationship
// from the durable mapping.
func verifyMigrationOperationProofs(ctx context.Context, r repository.Reader, base, candidate repository.Snapshot, plan migrationPlan) error {
	for _, item := range plan.Items {
		id, err := domain.ParseAssetID(item.AssetID)
		if err != nil {
			return fmt.Errorf("%w: invalid migration mapping asset id", domain.ErrRepositoryCorrupt)
		}
		asset, ok := base.Assets[id]
		if !ok {
			continue
		}
		newPath := repository.AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext)
		if item.Namespace != asset.Namespace || item.Object != asset.Object || item.NewPath != newPath {
			return fmt.Errorf("%w: migration mapping does not match asset %s", domain.ErrRepositoryCorrupt, id)
		}
		oldPath := repository.ObjectPath(asset)
		if oldPath != newPath {
			oldEntry, ok := entryAt(base.Entries, oldPath)
			if !ok || oldEntry.SHA != item.Blob {
				return fmt.Errorf("%w: source blob for %s differs from the mapping", domain.ErrRepositoryCorrupt, id)
			}
			if err := verifyEntrySHA256(ctx, r, oldEntry, item.SHA256); err != nil {
				return err
			}
		}
		target, ok := entryAt(candidate.Entries, newPath)
		if !ok || target.SHA != item.Blob {
			return fmt.Errorf("%w: target blob for %s differs from the mapping", domain.ErrRepositoryCorrupt, id)
		}
		if err := verifyEntrySHA256(ctx, r, target, item.SHA256); err != nil {
			return err
		}
	}
	return nil
}

func planObjects(base repository.Snapshot, plan migrationPlan) map[string]migrationPlanItem {
	allowed := make(map[string]migrationPlanItem)
	for _, item := range plan.Items {
		id, err := domain.ParseAssetID(item.AssetID)
		if err != nil {
			continue
		}
		asset, ok := base.Assets[id]
		if !ok || item.Namespace != asset.Namespace || item.Object != asset.Object {
			continue
		}
		if item.NewPath != repository.AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext) {
			continue
		}
		allowed[item.OldPath] = item
	}
	return allowed
}

// ---------------------------------------------------------------------------
// Candidate validation. These run before a journal is saved and again during
// recovery, so a corrupt journal cannot publish an arbitrary tree.

func validateMigrationObjects(ctx context.Context, r repository.Reader, base, candidate repository.Snapshot) error {
	if err := sameManifest(base.Manifest, candidate.Manifest); err != nil {
		return err
	}
	if err := sameAssetMetadata(base.Assets, candidate.Assets); err != nil {
		return err
	}
	expected, err := leafEntries(base.Entries)
	if err != nil {
		return err
	}
	for _, asset := range base.Assets {
		newPath := repository.AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext)
		oldPath := repository.ObjectPath(asset)
		if oldPath == newPath {
			continue
		}
		oldEntry, ok := entryAt(base.Entries, oldPath)
		if !ok {
			return fmt.Errorf("%w: base object %s is missing", domain.ErrRepositoryCorrupt, oldPath)
		}
		entry, existed := expected[newPath]
		if !existed {
			entry, ok = entryAt(candidate.Entries, newPath)
			if !ok {
				return fmt.Errorf("%w: new object %s is missing", domain.ErrRepositoryCorrupt, newPath)
			}
			expected[newPath] = entry
		}
		// A new path must point at the exact immutable source blob.
		if entry.SHA != oldEntry.SHA {
			return fmt.Errorf("%w: new object %s does not reuse the source blob", domain.ErrRepositoryCorrupt, newPath)
		}
		if err := verifyEntryObject(ctx, r, entry, asset.Object); err != nil {
			return err
		}
	}
	return compareLeafEntries(expected, candidate.Entries)
}

func validateMigrationMetadata(ctx context.Context, r repository.Reader, base, candidate repository.Snapshot) error {
	wantManifest := base.Manifest
	wantManifest.FormatVersion = 3
	if err := sameManifest(wantManifest, candidate.Manifest); err != nil {
		return err
	}
	expectedAssets := make(map[domain.AssetID]domain.Asset, len(base.Assets))
	expectedEntries, err := leafEntries(base.Entries)
	if err != nil {
		return err
	}
	if base.Manifest.FormatVersion != 3 {
		encoded, err := repository.EncodeManifest(wantManifest)
		if err != nil {
			return err
		}
		entry, ok := entryAt(candidate.Entries, repository.ManifestPath)
		if !ok {
			return fmt.Errorf("%w: upgraded manifest is missing", domain.ErrRepositoryCorrupt)
		}
		if err := verifyEntryBytes(ctx, r, entry, encoded); err != nil {
			return err
		}
		expectedEntries[repository.ManifestPath] = entry
	}
	for id, asset := range base.Assets {
		updated := asset
		newPath := repository.AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext)
		if repository.ObjectPath(asset) != newPath {
			updated.Path = newPath
			encoded, err := repository.EncodeAsset(updated)
			if err != nil {
				return err
			}
			entry, ok := entryAt(candidate.Entries, repository.AssetPath(id))
			if !ok {
				return fmt.Errorf("%w: migrated metadata %s is missing", domain.ErrRepositoryCorrupt, id)
			}
			if err := verifyEntryBytes(ctx, r, entry, encoded); err != nil {
				return err
			}
			expectedEntries[repository.AssetPath(id)] = entry
		}
		objectEntry, ok := entryAt(candidate.Entries, newPath)
		if !ok {
			return fmt.Errorf("%w: new object %s is missing", domain.ErrRepositoryCorrupt, newPath)
		}
		if err := verifyEntryObject(ctx, r, objectEntry, asset.Object); err != nil {
			return err
		}
		expectedAssets[id] = updated
	}
	if err := sameAssetMetadata(expectedAssets, candidate.Assets); err != nil {
		return err
	}
	return compareLeafEntries(expectedEntries, candidate.Entries)
}

func validatePruneCandidate(ctx context.Context, r repository.Reader, base, candidate repository.Snapshot, allowed map[string]migrationPlanItem, retention []byte) error {
	if err := sameManifest(base.Manifest, candidate.Manifest); err != nil {
		return err
	}
	if err := sameAssetMetadata(base.Assets, candidate.Assets); err != nil {
		return err
	}
	expected, err := leafEntries(base.Entries)
	if err != nil {
		return err
	}
	if len(retention) > 0 {
		entry, ok := entryAt(candidate.Entries, migrationRetentionPath)
		if !ok {
			return fmt.Errorf("%w: prune retention marker is missing", domain.ErrRepositoryCorrupt)
		}
		if err := verifyEntryBytes(ctx, r, entry, retention); err != nil {
			return err
		}
		expected[migrationRetentionPath] = entry
	}
	actual, err := leafEntries(candidate.Entries)
	if err != nil {
		return err
	}
	for path, entry := range actual {
		if _, ok := expected[path]; !ok {
			return fmt.Errorf("%w: prune added unrelated path %s", domain.ErrRepositoryCorrupt, path)
		}
		if expected[path] != entry {
			return fmt.Errorf("%w: prune changed path %s", domain.ErrRepositoryCorrupt, path)
		}
	}
	for path := range expected {
		if _, ok := actual[path]; ok {
			continue
		}
		item, ok := allowed[path]
		if !ok {
			return fmt.Errorf("%w: prune removed unmapped path %s", domain.ErrRepositoryCorrupt, path)
		}
		entry, exists := entryAt(base.Entries, path)
		if !exists {
			return fmt.Errorf("%w: prune removed a missing path %s", domain.ErrRepositoryCorrupt, path)
		}
		if err := verifyMappedObject(ctx, r, entry, item); err != nil {
			return err
		}
	}
	byID := make(map[domain.AssetID]migrationPlanItem, len(allowed))
	for _, item := range allowed {
		id, err := domain.ParseAssetID(item.AssetID)
		if err != nil {
			return fmt.Errorf("%w: invalid migration mapping asset id", domain.ErrRepositoryCorrupt)
		}
		byID[id] = item
	}
	for _, asset := range base.Assets {
		newPath := repository.AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext)
		if asset.Path != newPath {
			return fmt.Errorf("%w: prune retained an unmigrated asset %s", domain.ErrRepositoryCorrupt, asset.ID)
		}
		entry, ok := entryAt(candidate.Entries, newPath)
		if !ok {
			return fmt.Errorf("%w: prune removed a live object %s", domain.ErrRepositoryCorrupt, newPath)
		}
		if item, ok := byID[asset.ID]; ok && item.NewPath == newPath && item.Object == asset.Object {
			if err := verifyMappedObject(ctx, r, entry, item); err != nil {
				return err
			}
		} else if err := verifyEntryObject(ctx, r, entry, asset.Object); err != nil {
			return err
		}
	}
	return nil
}

func sameManifest(a, b domain.Repository) error {
	left, err := repository.EncodeManifest(a)
	if err != nil {
		return err
	}
	right, err := repository.EncodeManifest(b)
	if err != nil {
		return err
	}
	if !bytes.Equal(left, right) {
		return fmt.Errorf("%w: migration changed the repository manifest", domain.ErrRepositoryCorrupt)
	}
	return nil
}

func sameAssetMetadata(a, b map[domain.AssetID]domain.Asset) error {
	if len(a) != len(b) {
		return fmt.Errorf("%w: asset set differs", domain.ErrRepositoryCorrupt)
	}
	for id, left := range a {
		right, ok := b[id]
		if !ok {
			return fmt.Errorf("%w: asset %s is missing", domain.ErrRepositoryCorrupt, id)
		}
		lb, err := repository.EncodeAsset(left)
		if err != nil {
			return err
		}
		rb, err := repository.EncodeAsset(right)
		if err != nil {
			return err
		}
		if !bytes.Equal(lb, rb) {
			return fmt.Errorf("%w: asset %s metadata differs", domain.ErrRepositoryCorrupt, id)
		}
	}
	return nil
}

func leafEntries(entries []repository.GitEntry) (map[string]repository.GitEntry, error) {
	out := make(map[string]repository.GitEntry, len(entries))
	for _, entry := range entries {
		if entry.Type == "tree" {
			continue
		}
		if previous, ok := out[entry.Path]; ok && previous != entry {
			return nil, fmt.Errorf("%w: contradictory entries for %s", domain.ErrRepositoryCorrupt, entry.Path)
		}
		out[entry.Path] = entry
	}
	return out, nil
}

func compareLeafEntries(expected map[string]repository.GitEntry, entries []repository.GitEntry) error {
	actual, err := leafEntries(entries)
	if err != nil {
		return err
	}
	if len(expected) != len(actual) {
		return fmt.Errorf("%w: candidate contains unexpected or missing paths", domain.ErrRepositoryCorrupt)
	}
	for path, want := range expected {
		got, ok := actual[path]
		if !ok {
			return fmt.Errorf("%w: candidate is missing path %s", domain.ErrRepositoryCorrupt, path)
		}
		if got != want {
			return fmt.Errorf("%w: candidate changed unrelated path %s", domain.ErrRepositoryCorrupt, path)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Public target verification.

func verifyPublicObject(ctx context.Context, raw string, object domain.ContentObject) error {
	var last error
	for attempt := range 3 {
		if attempt > 0 {
			timer := time.NewTimer(time.Duration(attempt) * 500 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("%w: %v", domain.ErrDeliveryUnavailable, ctx.Err())
			case <-timer.C:
			}
		}
		last = fetchPublicObject(ctx, raw, object)
		if last == nil {
			return nil
		}
	}
	return fmt.Errorf("%w: %s: %v", domain.ErrDeliveryUnavailable, raw, last)
}

func fetchPublicObject(ctx context.Context, raw string, object domain.ContentObject) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept-Encoding", "identity")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %s", resp.Status)
	}
	body := io.LimitReader(resp.Body, object.Size+1)
	if object.Hash == (domain.ObjectHash{}) {
		return fmt.Errorf("missing SHA-256")
	}
	digest := sha256.New()
	size, err := io.Copy(digest, body)
	if err != nil {
		return err
	}
	if size != object.Size {
		return fmt.Errorf("size %d, want %d", size, object.Size)
	}
	if !bytes.Equal(digest.Sum(nil), object.Hash[:]) {
		return fmt.Errorf("sha256 differs")
	}
	return nil
}

// migrationRetentionPath is committed with prune so every workspace/device
// observes the same R2 retention set. It is not asset metadata and readSnapshot
// ignores it.
const migrationRetentionPath = ".touko/migration-retained.json"

type migrationRetention struct {
	Version      int      `json:"version"`
	RepositoryID string   `json:"repository_id"`
	Keys         []string `json:"keys"`
}

func encodeMigrationRetention(repositoryID domain.RepositoryID, keys []string) ([]byte, error) {
	keys = uniqueSorted(append([]string(nil), keys...))
	data, err := json.MarshalIndent(migrationRetention{Version: 1, RepositoryID: repositoryID.String(), Keys: keys}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func snapshotRetentionKeys(ctx context.Context, r repository.Reader, snap repository.Snapshot, repositoryID domain.RepositoryID) (map[string]bool, error) {
	out := make(map[string]bool)
	entry, ok := entryAt(snap.Entries, migrationRetentionPath)
	if !ok {
		return out, nil
	}
	if entry.Type != "blob" || entry.Mode != "100644" || entry.Size < 0 || entry.Size > 1<<20 {
		return nil, fmt.Errorf("%w: invalid migration retention entry", domain.ErrRepositoryCorrupt)
	}
	body, err := r.OpenBlob(ctx, repository.BlobID(entry.SHA))
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(body, 1<<20+1))
	closeErr := body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	var retention migrationRetention
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&retention); err != nil {
		return nil, fmt.Errorf("%w: invalid migration retention: %v", domain.ErrRepositoryCorrupt, err)
	}
	if retention.Version != 1 || (retention.RepositoryID != "" && retention.RepositoryID != repositoryID.String()) {
		return nil, fmt.Errorf("%w: migration retention belongs to another repository", domain.ErrRepositoryCorrupt)
	}
	for _, key := range retention.Keys {
		if err := domain.ValidateRepositoryPath(key); err != nil {
			return nil, fmt.Errorf("%w: retention key: %v", domain.ErrRepositoryCorrupt, err)
		}
		out[key] = true
	}
	return out, nil
}

// retainedR2Keys is deliberately conservative and cross-device: any blob that
// still exists in the authoritative tree is retained, and the committed prune
// marker protects paths removed from Git. The local plan is an extra guard.
func (s *Service) retainedR2Keys(ctx context.Context, r repository.Reader, snap repository.Snapshot) (map[string]bool, error) {
	out, err := snapshotRetentionKeys(ctx, r, snap, snap.Manifest.ID)
	if err != nil {
		return nil, err
	}
	for _, entry := range snap.Entries {
		if entry.Type == "blob" {
			out[entry.Path] = true
		}
	}
	plan, found, err := s.readMigrationPlan(ctx)
	if err != nil {
		return nil, err
	}
	if found {
		for _, item := range plan.Items {
			if item.OldPath != item.NewPath {
				out[item.OldPath] = true
			}
		}
	}
	return out, nil
}

func retentionForPrune(ctx context.Context, r repository.Reader, base repository.Snapshot, repositoryID domain.RepositoryID, plan migrationPlan) ([]byte, error) {
	keys, err := snapshotRetentionKeys(ctx, r, base, repositoryID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(keys)+len(plan.Items))
	for key := range keys {
		out = append(out, key)
	}
	for _, item := range plan.Items {
		out = append(out, item.OldPath)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return encodeMigrationRetention(repositoryID, out)
}

// ---------------------------------------------------------------------------
// Durable migration plan.

func (s *Service) migrationPlanPath() string {
	return filepath.Join(s.Dir, "migration", "plan.json")
}

func (s *Service) readMigrationPlan(ctx context.Context) (migrationPlan, bool, error) {
	if err := ctx.Err(); err != nil {
		return migrationPlan{}, false, err
	}
	data, err := os.ReadFile(s.migrationPlanPath())
	if errors.Is(err, os.ErrNotExist) {
		return migrationPlan{Version: migrationPlanVersion, Items: []migrationPlanItem{}}, false, nil
	}
	if err != nil {
		return migrationPlan{}, false, err
	}
	var plan migrationPlan
	if err = json.Unmarshal(data, &plan); err != nil {
		return migrationPlan{}, false, fmt.Errorf("%w: invalid migration mapping: %v", domain.ErrRepositoryCorrupt, err)
	}
	if err = validateMigrationPlan(plan); err != nil {
		return migrationPlan{}, false, err
	}
	return plan, true, nil
}

func validateMigrationPlan(plan migrationPlan) error {
	if plan.Version != migrationPlanVersion {
		return fmt.Errorf("%w: unsupported migration mapping version %d", domain.ErrRepositoryCorrupt, plan.Version)
	}
	if _, err := domain.ParseRepositoryID(plan.RepositoryID); err != nil {
		return fmt.Errorf("%w: invalid migration mapping repository id", domain.ErrRepositoryCorrupt)
	}
	seen := make(map[string]bool, len(plan.Items))
	for _, item := range plan.Items {
		id, err := domain.ParseAssetID(item.AssetID)
		if err != nil {
			return fmt.Errorf("%w: invalid migration mapping asset id", domain.ErrRepositoryCorrupt)
		}
		if seen[item.AssetID] {
			return fmt.Errorf("%w: duplicate migration mapping for %s", domain.ErrRepositoryCorrupt, item.AssetID)
		}
		seen[item.AssetID] = true
		if err := domain.ValidateRepositoryPath(item.OldPath); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrRepositoryCorrupt, err)
		}
		if err := domain.ValidateRepositoryPath(item.NewPath); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrRepositoryCorrupt, err)
		}
		if string(item.Namespace) == "" || item.OldPath == item.NewPath ||
			item.NewPath != repository.AssetObjectPath(item.Namespace, id, item.Object.Ext) {
			return fmt.Errorf("%w: inconsistent migration mapping for %s", domain.ErrRepositoryCorrupt, item.AssetID)
		}
		if err := item.Object.Validate(); err != nil {
			return fmt.Errorf("%w: migration mapping object: %v", domain.ErrRepositoryCorrupt, err)
		}
		if item.Object.Hash == (domain.ObjectHash{}) || item.SHA256 != item.Object.Hash.String() {
			return fmt.Errorf("%w: migration mapping for %s has an inconsistent source proof", domain.ErrRepositoryCorrupt, item.AssetID)
		}
		if len(item.SHA256) != 64 || item.Blob == "" {
			return fmt.Errorf("%w: migration mapping for %s has no source proof", domain.ErrRepositoryCorrupt, item.AssetID)
		}
		if _, err := hex.DecodeString(item.SHA256); err != nil {
			return fmt.Errorf("%w: migration mapping for %s has an invalid source proof", domain.ErrRepositoryCorrupt, item.AssetID)
		}
	}
	return nil
}

func (s *Service) mergeMigrationPlan(ctx context.Context, repositoryID domain.RepositoryID, additions []migrationPlanItem) (migrationPlan, error) {
	existing, found, err := s.readMigrationPlan(ctx)
	if err != nil {
		return migrationPlan{}, err
	}
	if !found {
		existing = migrationPlan{Version: migrationPlanVersion}
	}
	if existing.RepositoryID != "" && existing.RepositoryID != repositoryID.String() {
		return migrationPlan{}, fmt.Errorf("%w: migration mapping belongs to another repository", domain.ErrConflict)
	}
	existing.RepositoryID = repositoryID.String()
	items := make(map[string]migrationPlanItem, len(existing.Items)+len(additions))
	for _, item := range existing.Items {
		items[item.AssetID] = item
	}
	for _, item := range additions {
		items[item.AssetID] = item
	}
	merged := migrationPlan{Version: migrationPlanVersion, RepositoryID: existing.RepositoryID, Items: make([]migrationPlanItem, 0, len(items))}
	for _, item := range items {
		merged.Items = append(merged.Items, item)
	}
	sort.Slice(merged.Items, func(i, j int) bool { return merged.Items[i].AssetID < merged.Items[j].AssetID })
	if err = validateMigrationPlan(merged); err != nil {
		return migrationPlan{}, err
	}
	return merged, nil
}

func (s *Service) writeMigrationPlan(ctx context.Context, plan migrationPlan) error {
	if err := validateMigrationPlan(plan); err != nil {
		return err
	}
	dir := filepath.Dir(s.migrationPlanPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(ctx, s.migrationPlanPath(), append(data, '\n'), 0o600)
}



// ---------------------------------------------------------------------------
// Shared helpers.

func sourceProof(ctx context.Context, r repository.Reader, entry repository.GitEntry, object domain.ContentObject) (migrationProof, error) {
	if object.Hash == (domain.ObjectHash{}) {
		return migrationProof{}, fmt.Errorf("%w: missing SHA-256", domain.ErrRepositoryCorrupt)
	}
	if err := repository.VerifyObject(ctx, r, repository.BlobID(entry.SHA), object); err != nil {
		return migrationProof{}, err
	}
	return migrationProof{SHA256: object.Hash.String(), Blob: entry.SHA}, nil
}

func blobSHA256(ctx context.Context, r repository.Reader, entry repository.GitEntry) (string, error) {
	body, err := r.OpenBlob(ctx, repository.BlobID(entry.SHA))
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	size, err := io.Copy(digest, io.LimitReader(body, entry.Size+1))
	closeErr := body.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if size != entry.Size {
		return "", fmt.Errorf("%w: blob %s size differs", domain.ErrRepositoryCorrupt, entry.Path)
	}
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}

func verifyEntrySHA256(ctx context.Context, r repository.Reader, entry repository.GitEntry, want string) error {
	got, err := blobSHA256(ctx, r, entry)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%w: blob %s bytes differ from the recorded source", domain.ErrRepositoryCorrupt, entry.Path)
	}
	return nil
}

func verifyMappedObject(ctx context.Context, r repository.Reader, entry repository.GitEntry, item migrationPlanItem) error {
	if err := checkBlobEntry(entry, item.Object.Size); err != nil {
		return err
	}
	if item.Blob != "" && entry.SHA != item.Blob {
		return fmt.Errorf("%w: mapped object %s is a different Git blob", domain.ErrRepositoryCorrupt, entry.Path)
	}
	if err := verifyEntrySHA256(ctx, r, entry, item.SHA256); err != nil {
		return err
	}
	return repository.VerifyObject(ctx, r, repository.BlobID(entry.SHA), item.Object)
}

func checkBlobEntry(entry repository.GitEntry, size int64) error {
	if entry.Type != "blob" || (entry.Mode != "100644" && entry.Mode != "100755") || entry.Size != size {
		return fmt.Errorf("%w: invalid object entry %s", domain.ErrRepositoryCorrupt, entry.Path)
	}
	return nil
}

func verifyEntryObject(ctx context.Context, r repository.Reader, entry repository.GitEntry, object domain.ContentObject) error {
	if err := checkBlobEntry(entry, object.Size); err != nil {
		return err
	}
	if object.Hash == (domain.ObjectHash{}) {
		return fmt.Errorf("%w: missing SHA-256", domain.ErrRepositoryCorrupt)
	}
	return repository.VerifyObject(ctx, r, repository.BlobID(entry.SHA), object)
}

func entryBytes(ctx context.Context, r repository.Reader, entry repository.GitEntry) ([]byte, error) {
	if entry.Type != "blob" || entry.Size < 0 || entry.Size > 1<<20 {
		return nil, fmt.Errorf("%w: invalid metadata entry %s", domain.ErrRepositoryCorrupt, entry.Path)
	}
	body, err := r.OpenBlob(ctx, repository.BlobID(entry.SHA))
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(body, entry.Size+1))
	closeErr := body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(data)) != entry.Size {
		return nil, fmt.Errorf("%w: metadata length differs at %s", domain.ErrRepositoryCorrupt, entry.Path)
	}
	return data, nil
}

func verifyEntryBytes(ctx context.Context, r repository.Reader, entry repository.GitEntry, want []byte) error {
	if err := checkBlobEntry(entry, int64(len(want))); err != nil {
		return err
	}
	data, err := entryBytes(ctx, r, entry)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, want) {
		return fmt.Errorf("%w: metadata bytes differ at %s", domain.ErrRepositoryCorrupt, entry.Path)
	}
	return nil
}

func entryAt(entries []repository.GitEntry, path string) (repository.GitEntry, bool) {
	for _, entry := range entries {
		if entry.Path == path {
			return entry, true
		}
	}
	return repository.GitEntry{}, false
}

func sortedIDs(values map[domain.AssetID]bool) []domain.AssetID {
	out := make([]domain.AssetID, 0, len(values))
	for id := range values {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func uniqueSorted(values []string) []string {
	sort.Strings(values)
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}

type urlBuilder struct {
	location repository.Location
	r2Base   string
}

func (b urlBuilder) github(objectPath string) string {
	url, err := delivery.BuildURL(b.location, objectPath, delivery.GitHub)
	if err != nil {
		return ""
	}
	return url
}

func (b urlBuilder) r2(objectPath string) string {
	if b.r2Base == "" {
		return ""
	}
	url, err := delivery.BuildCloudflareURL(b.r2Base, objectPath)
	if err != nil {
		return ""
	}
	return url
}

// WriteURLMapping writes migration/url-mapping.csv and .json under dir. Both
// files are replaced atomically so a crash never leaves a half-written mapping.
func WriteURLMapping(ctx context.Context, dir string, items []MigrateItem) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	csvPath := filepath.Join(dir, "url-mapping.csv")
	jsonPath := filepath.Join(dir, "url-mapping.json")
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	header := []string{"asset_id", "old_path", "new_path", "old_github_url", "new_github_url", "old_r2_url", "new_r2_url", "size_bytes", "status", "error", "sha256"}
	if err := w.Write(header); err != nil {
		return "", "", err
	}
	for _, item := range items {
		row := []string{item.AssetID, item.OldPath, item.NewPath, item.OldGitHub, item.NewGitHub, item.OldR2, item.NewR2, strconv.FormatInt(item.SizeBytes, 10), item.Status, item.Error, item.SHA256}
		if err := w.Write(row); err != nil {
			return "", "", err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return "", "", err
	}
	if err := atomicfile.Write(ctx, csvPath, buf.Bytes(), 0o600); err != nil {
		return "", "", err
	}
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return "", "", err
	}
	if err := atomicfile.Write(ctx, jsonPath, append(data, '\n'), 0o600); err != nil {
		return "", "", err
	}
	return csvPath, jsonPath, nil
}
