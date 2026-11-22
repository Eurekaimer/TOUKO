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
	toukoimage "github.com/Eurekaimer/TOUKO/internal/image"
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
type MigrateAlias struct {
	Path   string `json:"path"`
	GitHub string `json:"github_url,omitempty"`
	R2     string `json:"r2_url,omitempty"`
}

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
	// Aliases are older public paths for the same bytes. They are expanded into
	// extra mapping rows so link rewriting does not miss a previously used URL.
	Aliases []MigrateAlias `json:"aliases,omitempty"`
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
	// PriorPaths are older aliases superseded by an earlier migration of the
	// same asset. They are never published and are kept only so prune and R2
	// retention keep honoring URLs that predate this run.
	PriorPaths []string `json:"prior_paths,omitempty"`
	// SHA256 and Blob preserve the exact source proof across recovery. SHA256
	// is empty only when the asset was recorded without a digest; Blob then
	// carries the immutable Git identity instead.
	SHA256 string `json:"sha256"`
	Blob   string `json:"blob"`
}

// migrationUpdate is the exact move of one asset: the bytes at FromPath must
// be re-published at ToPath without another upload.
type migrationUpdate struct {
	AssetID  domain.AssetID
	FromPath string
	ToPath   string
	Item     migrationPlanItem
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
	updates     map[domain.AssetID]migrationUpdate
	needObjects bool
	needMeta    bool
}

// MigratePaths rewrites every managed legacy asset to its root timestamp name
// in two published phases. Phase one adds the new object entries while metadata
// still points at the old paths; phase two switches metadata in one commit.
// Old objects and old URLs therefore survive every failure, and only prune can
// remove the exact entries recorded in the durable migration plan. Assets
// that already use a timestamp name keep their current path.
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
			return result, fmt.Errorf("%w: prune requires every managed asset to use its timestamp name; run migrate paths first", domain.ErrConflict)
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
	if len(analysis.planItems) > 0 {
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
			targets = analysis.targetAssets()
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

// changedAssets returns the assets whose object must be published at a new
// path before metadata switches. Each copy carries its target in Path so the
// Cloudflare bridge mirrors the exact destination.
func (a migrationAnalysis) changedAssets() []domain.Asset {
	out := make([]domain.Asset, 0, len(a.changed))
	for _, asset := range a.assets {
		if a.changed[asset.ID] {
			asset.Path = a.updates[asset.ID].ToPath
			out = append(out, asset)
		}
	}
	return out
}

// targetAssets returns every asset with its final path in Path. Verification
// of a full gallery needs the destinations, not the metadata that is about to
// be replaced.
func (a migrationAnalysis) targetAssets() []domain.Asset {
	out := make([]domain.Asset, 0, len(a.assets))
	for _, asset := range a.assets {
		if update, ok := a.updates[asset.ID]; ok {
			asset.Path = update.ToPath
		}
		out = append(out, asset)
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
		updates:    make(map[domain.AssetID]migrationUpdate),
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
	if base.Manifest.FormatVersion != 3 {
		// Older clients could delete legacy R2 keys. Any run is the right moment
		// to install the barrier, even when no path moves.
		a.needMeta = true
	}
	targets := make(map[domain.AssetID]string, len(ids))
	reserved := make(map[string]domain.AssetID, len(ids))
	// Timestamp names are final. Reserve them first so a generated filename can
	// never collide with a live asset.
	for _, id := range ids {
		asset := base.Assets[id]
		a.assets = append(a.assets, asset)
		if !repository.IsTimestampObjectPath(asset.Path, asset.Object.Ext) {
			continue
		}
		if other, ok := reserved[asset.Path]; ok && other != id {
			return a, fmt.Errorf("%w: two assets share target path %s", domain.ErrRepositoryCorrupt, asset.Path)
		}
		reserved[asset.Path] = id
		targets[id] = asset.Path
	}
	// Assign one deterministic root filename per legacy placement.
	for _, id := range ids {
		if _, final := targets[id]; final {
			continue
		}
		asset := base.Assets[id]
		oldPath, managed, err := migrationSourcePath(asset)
		if err != nil {
			return a, err
		}
		if !managed {
			targets[id] = asset.Path
			continue
		}
		source, hasSource := entryAt(base.Entries, oldPath)
		target := ""
		if planned, ok := planByID[id]; ok &&
			planned.Namespace == asset.Namespace && planned.Object == asset.Object &&
			planned.OldPath == oldPath &&
			repository.IsTimestampObjectPath(planned.NewPath, asset.Object.Ext) &&
			migrationTargetAvailable(planned.NewPath, id, reserved, base.Entries, source, hasSource) {
			// An interrupted run already recorded this exact target.
			target = planned.NewPath
		}
		if target == "" {
			target, err = migrationTimestampTarget(asset, id, reserved, base.Entries, source, hasSource)
			if err != nil {
				return a, err
			}
		}
		if other, ok := reserved[target]; ok && other != id {
			return a, fmt.Errorf("%w: two assets share target path %s", domain.ErrRepositoryCorrupt, target)
		}
		reserved[target] = id
		targets[id] = target
	}
	for _, id := range ids {
		asset := base.Assets[id]
		target := targets[id]
		planned, hasPlan := planByID[id]
		if hasPlan && (planned.Namespace != asset.Namespace || planned.Object != asset.Object) {
			return a, fmt.Errorf("%w: migration mapping does not match asset %s", domain.ErrConflict, id)
		}
		item := MigrateItem{
			AssetID: asset.ID.String(), OldPath: target, NewPath: target,
			SizeBytes: asset.Object.Size, Status: "unchanged", SHA256: objectSHA(asset.Object),
		}
		if asset.Path != target {
			oldPath := repository.ObjectPath(asset)
			if hasPlan && planned.OldPath != oldPath && planned.NewPath != asset.Path {
				return a, fmt.Errorf("%w: migration mapping records old path %s for asset %s, but metadata uses %s", domain.ErrConflict, planned.OldPath, id, oldPath)
			}
			source, ok := entryAt(base.Entries, oldPath)
			if !ok {
				return a, fmt.Errorf("%w: base object %s is missing", domain.ErrRepositoryCorrupt, oldPath)
			}
			if err := checkBlobEntry(source, asset.Object.Size); err != nil {
				return a, err
			}
			proof, err := sourceProof(ctx, r, source, asset.Object)
			if err != nil {
				return a, err
			}
			// The row reports the oldest URL this asset ever had so an operator
			// rewriting documents does not miss the link that is actually out
			// there; immediate source aliases are kept in the plan.
			item.OldPath = migrationEarliestPath(planned, hasPlan, oldPath)
			plannedItem := migrationPlanItem{
				AssetID: id.String(), Namespace: asset.Namespace, OldPath: oldPath, NewPath: target,
				Object: asset.Object, PriorPaths: migrationPriorPaths(planned, hasPlan, oldPath),
				SHA256: proof.SHA256, Blob: proof.Blob,
			}
			item.SHA256 = proof.SHA256
			a.oldEntries[id] = source
			a.changed[id] = true
			a.planItems = append(a.planItems, plannedItem)
			a.updates[id] = migrationUpdate{AssetID: id, FromPath: oldPath, ToPath: target, Item: plannedItem}
			if existing, ok := entryAt(base.Entries, target); ok {
				// A resumed phase one must reuse the exact same immutable blob.
				// Same-size wrong bytes are never accepted as a completed target.
				if err := checkBlobEntry(existing, asset.Object.Size); err != nil {
					return a, err
				}
				if existing.SHA != source.SHA {
					return a, fmt.Errorf("%w: target path %s is occupied by different bytes", domain.ErrConflict, target)
				}
			} else {
				a.missing[id] = true
				a.needObjects = true
			}
			a.needMeta = true
			item.Status = "planned"
		} else if hasPlan {
			// A resumed run whose metadata already points at the timestamp name
			// reconstructs the original row from the durable mapping.
			if planned.NewPath != target {
				return a, fmt.Errorf("%w: migration mapping does not match asset %s", domain.ErrConflict, id)
			}
			if earliest := migrationEarliestPath(planned, true, target); earliest != target {
				item.OldPath = earliest
				item.Status = "migrated"
			}
		}
		item.OldGitHub, item.NewGitHub = helpers.github(item.OldPath), helpers.github(item.NewPath)
		item.OldR2, item.NewR2 = helpers.r2(item.OldPath), helpers.r2(item.NewPath)
		aliases := []string{item.OldPath}
		if a.changed[id] {
			aliases = append(aliases, repository.ObjectPath(asset))
		}
		if hasPlan {
			aliases = append(aliases, planned.OldPath)
			aliases = append(aliases, planned.PriorPaths...)
		}
		seenAliases := make(map[string]bool, len(aliases))
		for _, alias := range aliases {
			if alias == "" || alias == target || seenAliases[alias] {
				continue
			}
			seenAliases[alias] = true
			item.Aliases = append(item.Aliases, MigrateAlias{Path: alias, GitHub: helpers.github(alias), R2: helpers.r2(alias)})
		}
		a.items = append(a.items, item)
	}
	return a, nil
}

// migrationSourcePath reports the current placement and whether its filename
// still needs a timestamp.
func migrationSourcePath(asset domain.Asset) (string, bool, error) {
	if asset.Path == "" {
		if !asset.Object.HasHash() {
			return "", false, fmt.Errorf("%w: asset %s has neither a path nor a SHA-256", domain.ErrRepositoryCorrupt, asset.ID)
		}
		return repository.LegacyObjectPath(asset.Namespace, asset.Object), true, nil
	}
	return asset.Path, !repository.IsTimestampObjectPath(asset.Path, asset.Object.Ext), nil
}

// migrationTargetAvailable accepts a planned or generated filename only when no
// other asset owns it and the tree either has no entry there or already holds
// this asset's exact source blob (a resumed phase one).
func migrationTargetAvailable(candidate string, id domain.AssetID, reserved map[string]domain.AssetID, entries []repository.GitEntry, source repository.GitEntry, hasSource bool) bool {
	if candidate == "" {
		return false
	}
	if other, ok := reserved[candidate]; ok && other != id {
		return false
	}
	entry, exists := entryAt(entries, candidate)
	if !exists {
		return true
	}
	return hasSource && entry.Type == "blob" && entry.SHA == source.SHA && entry.Size == source.Size
}

const migrationTimestampAttempts = 10_000

// migrationTimestampTarget derives the root filename from the asset's creation
// time plus a stable collision suffix. An occupied path with different bytes is
// never clobbered; the next suffix is tried instead.
func migrationTimestampTarget(asset domain.Asset, id domain.AssetID, reserved map[string]domain.AssetID, entries []repository.GitEntry, source repository.GitEntry, hasSource bool) (string, error) {
	at := asset.CreatedAt.UTC()
	if at.IsZero() {
		return "", fmt.Errorf("%w: asset %s has no creation time", domain.ErrRepositoryCorrupt, id)
	}
	for collision := range migrationTimestampAttempts {
		candidate := repository.TimestampObjectPath(at, asset.Object.Ext, collision)
		if migrationTargetAvailable(candidate, id, reserved, entries, source, hasSource) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%w: no free timestamp path for asset %s", domain.ErrConflict, id)
}

// migrationPriorPaths keeps every older alias the durable mapping recorded, so
// prune and R2 retention still honor links that predate this run.
func migrationPriorPaths(planned migrationPlanItem, ok bool, current string) []string {
	if !ok {
		return nil
	}
	out := make([]string, 0, len(planned.PriorPaths)+1)
	seen := make(map[string]bool, len(planned.PriorPaths)+1)
	appendPath := func(p string) {
		if p == "" || p == current || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, p := range planned.PriorPaths {
		appendPath(p)
	}
	appendPath(planned.OldPath)
	if len(out) == 0 {
		return nil
	}
	return out
}

func migrationEarliestPath(planned migrationPlanItem, ok bool, fallback string) string {
	if !ok {
		return fallback
	}
	if len(planned.PriorPaths) > 0 {
		return planned.PriorPaths[0]
	}
	if planned.OldPath != "" {
		return planned.OldPath
	}
	return fallback
}

// mergeMigrationPriorPaths folds a superseded mapping into its replacement
// without dropping any URL an earlier run promised.
func mergeMigrationPriorPaths(item, previous migrationPlanItem) []string {
	out := make([]string, 0, len(item.PriorPaths)+len(previous.PriorPaths)+1)
	seen := make(map[string]bool, len(item.PriorPaths)+len(previous.PriorPaths)+1)
	appendPath := func(p string) {
		if p == "" || p == item.NewPath || p == item.OldPath || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, prior := range item.PriorPaths {
		appendPath(prior)
	}
	for _, prior := range previous.PriorPaths {
		appendPath(prior)
	}
	appendPath(previous.OldPath)
	if len(out) == 0 {
		return nil
	}
	return out
}

// objectSHA keeps a missing digest empty instead of rendering zero bytes as if
// they were an observed hash.
func objectSHA(object domain.ContentObject) string {
	if !object.HasHash() {
		return ""
	}
	return object.Hash.String()
}

func (s *Service) publishMigrationObjects(ctx context.Context, r repository.Git, st index.State, base repository.Snapshot, a migrationAnalysis) (repository.Snapshot, error) {
	changes := make([]repository.TreeChange, 0, len(a.missing))
	for _, id := range sortedIDs(a.missing) {
		asset := base.Assets[id]
		oldEntry, ok := a.oldEntries[id]
		if !ok {
			return repository.Snapshot{}, fmt.Errorf("%w: migration source for %s is unknown", domain.ErrRepositoryCorrupt, asset.ID)
		}
		update := a.updates[id]
		if update.ToPath == "" {
			return repository.Snapshot{}, fmt.Errorf("%w: migration target for %s is unknown", domain.ErrRepositoryCorrupt, asset.ID)
		}
		// Git blobs are immutable and content addressed: the new path can point
		// at the already verified old blob without another upload.
		blob := repository.BlobID(oldEntry.SHA)
		changes = append(changes, repository.TreeChange{
			Path: update.ToPath,
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
	candidateHead, err := r.CreateCommit(ctx, tree, base.Head, "migrate: add timestamped object copies")
	if err != nil {
		return repository.Snapshot{}, err
	}
	candidate, err := repository.ReadSnapshot(ctx, r, candidateHead)
	if err != nil {
		return repository.Snapshot{}, err
	}
	if err = validateMigrationObjects(ctx, r, base, candidate, a.updates); err != nil {
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
	published, err := s.publish(ctx, r, st, candidate, nil)
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
		updated.Path = a.updates[id].ToPath
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
	candidateHead, err := r.CreateCommit(ctx, tree, base.Head, "migrate: use timestamp names in object paths")
	if err != nil {
		return repository.Snapshot{}, err
	}
	candidate, err := repository.ReadSnapshot(ctx, r, candidateHead)
	if err != nil {
		return repository.Snapshot{}, err
	}
	if err = validateMigrationMetadata(ctx, r, base, candidate, a.updates); err != nil {
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
	published, err := s.publish(ctx, r, st, candidate, nil)
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
		mirror, err := s.cloudflareSyncLocked(ctx, r, CloudflareOptions{SuppressDeletes: true, OnlyExtra: true, ExtraAssets: assets})
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
		newPath := repository.ObjectPath(asset)
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
		newPath := a.updates[id].ToPath
		if asset.Path != newPath {
			return fmt.Errorf("%w: asset %s does not use its migrated path", domain.ErrRepositoryCorrupt, id)
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
	if proof.Blob == "" {
		return fmt.Errorf("%w: target %s has no immutable source blob", domain.ErrRepositoryCorrupt, entry.Path)
	}
	if entry.SHA != proof.Blob {
		return fmt.Errorf("%w: target %s is not the recorded source blob", domain.ErrRepositoryCorrupt, entry.Path)
	}
	if object.HasHash() {
		if proof.SHA256 != object.Hash.String() {
			return fmt.Errorf("%w: recorded source proof differs from metadata", domain.ErrRepositoryCorrupt)
		}
		return repository.VerifyObject(ctx, r, repository.BlobID(entry.SHA), object)
	}
	if proof.SHA256 != "" {
		return fmt.Errorf("%w: recorded source proof differs from metadata", domain.ErrRepositoryCorrupt)
	}
	// Without a recorded digest the immutable blob identity, the checked size
	// and the verified image description are the proof; a zero hash is never
	// treated as a digest.
	return repository.VerifyObject(ctx, r, repository.BlobID(entry.SHA), object)
}

func (s *Service) assetProof(ctx context.Context, r repository.Reader, snap repository.Snapshot, asset domain.Asset, plan map[domain.AssetID]migrationPlanItem) (migrationProof, error) {
	newPath := repository.ObjectPath(asset)
	if item, ok := plan[asset.ID]; ok {
		if item.NewPath != newPath || item.Object != asset.Object {
			return migrationProof{}, fmt.Errorf("%w: durable mapping does not match asset %s", domain.ErrRepositoryCorrupt, asset.ID)
		}
		return migrationProof{SHA256: item.SHA256, Blob: item.Blob}, nil
	}
	entry, ok := entryAt(snap.Entries, newPath)
	if !ok {
		return migrationProof{}, fmt.Errorf("%w: new object %s is not published", domain.ErrRepositoryCorrupt, newPath)
	}
	if err := checkBlobEntry(entry, asset.Object.Size); err != nil {
		return migrationProof{}, err
	}
	return migrationProof{SHA256: objectSHA(asset.Object), Blob: entry.SHA}, nil
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
// after every managed asset uses a verified timestamp placement. It never
// derives candidates from hashes and never touches metadata or unrelated
// objects.
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
	// Every managed asset must already use a verified timestamp placement.
	// Custom upload directories are not migration's to move or judge.
	referenced := make(map[string]bool, len(base.Assets))
	for id, asset := range base.Assets {
		_, managed, err := migrationSourcePath(asset)
		if err != nil {
			return result, err
		}
		if managed {
			return result, fmt.Errorf("%w: asset %s is not migrated; run migrate paths first", domain.ErrConflict, id)
		}
		livePath := repository.ObjectPath(asset)
		entry, ok := entryAt(base.Entries, livePath)
		if !ok {
			return result, fmt.Errorf("%w: live object %s is missing", domain.ErrRepositoryCorrupt, livePath)
		}
		if item, ok := planByID[id.String()]; ok && item.NewPath == livePath && item.Object == asset.Object {
			if err := verifyMappedObject(ctx, r, entry, item); err != nil {
				return result, err
			}
		} else if err := verifyEntryObject(ctx, r, entry, asset.Object); err != nil {
			return result, err
		}
		referenced[livePath] = true
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
		if item.Namespace != asset.Namespace || item.Object != asset.Object || item.NewPath != asset.Path {
			return result, fmt.Errorf("%w: migration mapping does not match asset %s", domain.ErrConflict, id)
		}
		if item.OldPath == item.NewPath {
			return result, fmt.Errorf("%w: migration mapping has an empty move for %s", domain.ErrRepositoryCorrupt, id)
		}
		// Prior aliases from earlier migrations are deleted only under this
		// explicit prune, never by a normal run, and only when verified.
		for _, oldPath := range append([]string{item.OldPath}, item.PriorPaths...) {
			if oldPath == item.NewPath {
				return result, fmt.Errorf("%w: migration mapping has an empty move for %s", domain.ErrRepositoryCorrupt, id)
			}
			if referenced[oldPath] {
				return result, fmt.Errorf("%w: old path %s is still live", domain.ErrConflict, oldPath)
			}
			if entry, ok := entries[oldPath]; ok {
				if err := verifyMappedObject(ctx, r, entry, item); err != nil {
					return result, err
				}
				gitDeletions = append(gitDeletions, oldPath)
			}
			allowed[oldPath] = item
			r2Keys = append(r2Keys, oldPath)
		}
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
		candidateHead, err := r.CreateCommit(ctx, tree, base.Head, "migrate: remove superseded object paths")
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
		published, err := s.publish(ctx, r, st, candidate, nil)
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
	return s.publish(ctx, r, st, candidate, nil)
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
	return s.finalizeSnapshot(ctx, r, st, snap, nil)
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
		var plan migrationPlan
		var found bool
		plan, found, err = s.readMigrationPlan(ctx)
		if err == nil && !found {
			err = fmt.Errorf("%w: migration journal has no durable mapping", domain.ErrRepositoryCorrupt)
		}
		if err == nil {
			var updates map[domain.AssetID]migrationUpdate
			updates, err = migrationUpdates(base, plan)
			if err == nil && p.Operation == migrationObjectsOperation {
				err = validateMigrationObjects(ctx, r, base, candidate, updates)
			} else if err == nil {
				err = validateMigrationMetadata(ctx, r, base, candidate, updates)
			}
			if err == nil {
				err = verifyMigrationOperationProofs(ctx, r, base, candidate, updates)
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

// migrationUpdates derives the exact per-asset moves a plan promises for the
// current metadata. Assets whose metadata already points at the recorded target
// have no update; anything else must match the recorded source exactly.
func migrationUpdates(base repository.Snapshot, plan migrationPlan) (map[domain.AssetID]migrationUpdate, error) {
	updates := make(map[domain.AssetID]migrationUpdate, len(plan.Items))
	for _, item := range plan.Items {
		id, err := domain.ParseAssetID(item.AssetID)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid migration mapping asset id", domain.ErrRepositoryCorrupt)
		}
		asset, ok := base.Assets[id]
		if !ok {
			continue
		}
		if item.Namespace != asset.Namespace || item.Object != asset.Object {
			return nil, fmt.Errorf("%w: migration mapping does not match asset %s", domain.ErrRepositoryCorrupt, id)
		}
		if asset.Path == item.NewPath {
			continue
		}
		fromPath := repository.ObjectPath(asset)
		if fromPath != item.OldPath {
			return nil, fmt.Errorf("%w: migration mapping records old path %s for asset %s, but metadata uses %s", domain.ErrRepositoryCorrupt, item.OldPath, id, fromPath)
		}
		updates[id] = migrationUpdate{AssetID: id, FromPath: fromPath, ToPath: item.NewPath, Item: item}
	}
	return updates, nil
}

// verifyMigrationOperationProofs re-proves the exact source/copy relationship
// from the durable mapping.
func verifyMigrationOperationProofs(ctx context.Context, r repository.Reader, base, candidate repository.Snapshot, updates map[domain.AssetID]migrationUpdate) error {
	for id, update := range updates {
		item := update.Item
		source, ok := entryAt(base.Entries, update.FromPath)
		if !ok || source.SHA != item.Blob {
			return fmt.Errorf("%w: source blob for %s differs from the mapping", domain.ErrRepositoryCorrupt, id)
		}
		if item.SHA256 != "" {
			if err := verifyEntrySHA256(ctx, r, source, item.SHA256); err != nil {
				return err
			}
		}
		target, ok := entryAt(candidate.Entries, update.ToPath)
		if !ok || target.SHA != item.Blob {
			return fmt.Errorf("%w: target blob for %s differs from the mapping", domain.ErrRepositoryCorrupt, id)
		}
		if item.SHA256 != "" {
			if err := verifyEntrySHA256(ctx, r, target, item.SHA256); err != nil {
				return err
			}
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
		// Prune recovery sees the post-migration metadata the journal promised.
		if item.NewPath != asset.Path {
			continue
		}
		allowed[item.OldPath] = item
		for _, prior := range item.PriorPaths {
			allowed[prior] = item
		}
	}
	return allowed
}

// ---------------------------------------------------------------------------
// Candidate validation. These run before a journal is saved and again during
// recovery, so a corrupt journal cannot publish an arbitrary tree.

func validateMigrationObjects(ctx context.Context, r repository.Reader, base, candidate repository.Snapshot, updates map[domain.AssetID]migrationUpdate) error {
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
	for _, update := range updates {
		source, ok := entryAt(base.Entries, update.FromPath)
		if !ok {
			return fmt.Errorf("%w: base object %s is missing", domain.ErrRepositoryCorrupt, update.FromPath)
		}
		if err := verifyMappedObject(ctx, r, source, update.Item); err != nil {
			return err
		}
		entry, existed := expected[update.ToPath]
		if !existed {
			entry, ok = entryAt(candidate.Entries, update.ToPath)
			if !ok {
				return fmt.Errorf("%w: new object %s is missing", domain.ErrRepositoryCorrupt, update.ToPath)
			}
			expected[update.ToPath] = entry
		}
		// A new path must point at the exact immutable source blob.
		if entry.SHA != source.SHA {
			return fmt.Errorf("%w: new object %s does not reuse the source blob", domain.ErrRepositoryCorrupt, update.ToPath)
		}
		if err := verifyMappedObject(ctx, r, entry, update.Item); err != nil {
			return err
		}
	}
	return compareLeafEntries(expected, candidate.Entries)
}

func validateMigrationMetadata(ctx context.Context, r repository.Reader, base, candidate repository.Snapshot, updates map[domain.AssetID]migrationUpdate) error {
	wantManifest := base.Manifest
	wantManifest.FormatVersion = 3
	if err := sameManifest(wantManifest, candidate.Manifest); err != nil {
		return err
	}
	expectedAssets := make(map[domain.AssetID]domain.Asset, len(base.Assets))
	for id, asset := range base.Assets {
		expectedAssets[id] = asset
	}
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
	for id, update := range updates {
		asset, ok := expectedAssets[id]
		if !ok {
			return fmt.Errorf("%w: migration mapping names missing asset %s", domain.ErrRepositoryCorrupt, id)
		}
		updated := asset
		updated.Path = update.ToPath
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
		objectEntry, ok := entryAt(candidate.Entries, update.ToPath)
		if !ok {
			return fmt.Errorf("%w: new object %s is missing", domain.ErrRepositoryCorrupt, update.ToPath)
		}
		if err := verifyMappedObject(ctx, r, objectEntry, update.Item); err != nil {
			return err
		}
		expectedEntries[update.ToPath] = objectEntry
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
		livePath := repository.ObjectPath(asset)
		entry, ok := entryAt(candidate.Entries, livePath)
		if !ok {
			return fmt.Errorf("%w: prune removed a live object %s", domain.ErrRepositoryCorrupt, livePath)
		}
		if item, ok := byID[asset.ID]; ok && item.NewPath == livePath && item.Object == asset.Object {
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
	if !object.HasHash() {
		// No digest was recorded. Verify the declared size, format and
		// dimensions; a zero hash is never compared as an observed digest.
		if err := toukoimage.VerifyDescription(ctx, body, object); err != nil {
			return err
		}
		return nil
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
			for _, prior := range item.PriorPaths {
				out[prior] = true
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
		out = append(out, item.PriorPaths...)
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
		if string(item.Namespace) == "" || item.OldPath == item.NewPath {
			return fmt.Errorf("%w: inconsistent migration mapping for %s", domain.ErrRepositoryCorrupt, item.AssetID)
		}
		// New mappings use the readable root timestamp name. Earlier timestamp
		// and asset-id mappings stay readable so a resumed run can finish and
		// their old aliases keep protecting R2 keys.
		if !repository.IsTimestampObjectPath(item.NewPath, item.Object.Ext) &&
			!repository.IsLegacyTimestampObjectPath(item.NewPath, item.Object.Ext) &&
			item.NewPath != repository.AssetObjectPath(item.Namespace, id, item.Object.Ext) {
			return fmt.Errorf("%w: inconsistent migration mapping for %s", domain.ErrRepositoryCorrupt, item.AssetID)
		}
		if err := item.Object.Validate(); err != nil {
			return fmt.Errorf("%w: migration mapping object: %v", domain.ErrRepositoryCorrupt, err)
		}
		if item.Object.HasHash() {
			if item.SHA256 != item.Object.Hash.String() {
				return fmt.Errorf("%w: migration mapping for %s has an inconsistent source proof", domain.ErrRepositoryCorrupt, item.AssetID)
			}
		} else if item.SHA256 != "" {
			return fmt.Errorf("%w: migration mapping for %s has an inconsistent source proof", domain.ErrRepositoryCorrupt, item.AssetID)
		}
		if item.Blob == "" {
			return fmt.Errorf("%w: migration mapping for %s has no source proof", domain.ErrRepositoryCorrupt, item.AssetID)
		}
		if item.SHA256 != "" {
			if len(item.SHA256) != 64 {
				return fmt.Errorf("%w: migration mapping for %s has an invalid source proof", domain.ErrRepositoryCorrupt, item.AssetID)
			}
			if _, err := hex.DecodeString(item.SHA256); err != nil {
				return fmt.Errorf("%w: migration mapping for %s has an invalid source proof", domain.ErrRepositoryCorrupt, item.AssetID)
			}
		}
		seenPrior := make(map[string]bool, len(item.PriorPaths))
		for _, prior := range item.PriorPaths {
			if err := domain.ValidateRepositoryPath(prior); err != nil {
				return fmt.Errorf("%w: %v", domain.ErrRepositoryCorrupt, err)
			}
			if prior == item.NewPath || prior == item.OldPath || seenPrior[prior] {
				return fmt.Errorf("%w: inconsistent migration mapping for %s", domain.ErrRepositoryCorrupt, item.AssetID)
			}
			seenPrior[prior] = true
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
		if previous, ok := items[item.AssetID]; ok {
			if previous.Object != item.Object || previous.Namespace != item.Namespace {
				return migrationPlan{}, fmt.Errorf("%w: migration mapping for %s changed identity", domain.ErrConflict, item.AssetID)
			}
			// Never lose an older alias when this run supersedes its target.
			item.PriorPaths = mergeMigrationPriorPaths(item, previous)
		}
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
	if err := checkBlobEntry(entry, object.Size); err != nil {
		return migrationProof{}, err
	}
	if object.HasHash() {
		if err := repository.VerifyObject(ctx, r, repository.BlobID(entry.SHA), object); err != nil {
			return migrationProof{}, err
		}
		return migrationProof{SHA256: object.Hash.String(), Blob: entry.SHA}, nil
	}
	// No digest was recorded. Verify the image description and keep the
	// immutable Git blob id as the proof; a missing digest is never compared as
	// a zero digest.
	if err := repository.VerifyObject(ctx, r, repository.BlobID(entry.SHA), object); err != nil {
		return migrationProof{}, err
	}
	return migrationProof{Blob: entry.SHA}, nil
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
	if item.Blob == "" || entry.SHA != item.Blob {
		return fmt.Errorf("%w: mapped object %s is a different Git blob", domain.ErrRepositoryCorrupt, entry.Path)
	}
	if item.SHA256 != "" {
		if err := verifyEntrySHA256(ctx, r, entry, item.SHA256); err != nil {
			return err
		}
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

// urlMappingRows expands each asset's historical aliases into independent
// rows; the rewrite script matches one old path at a time.
func urlMappingRows(items []MigrateItem) []MigrateItem {
	rows := make([]MigrateItem, 0, len(items))
	for _, item := range items {
		seen := map[string]bool{}
		add := func(path, github, r2 string, primary bool) {
			if path == "" || seen[path] || (!primary && path == item.NewPath) {
				return
			}
			seen[path] = true
			row := item
			row.OldPath, row.OldGitHub, row.OldR2 = path, github, r2
			row.Aliases = nil
			rows = append(rows, row)
		}
		add(item.OldPath, item.OldGitHub, item.OldR2, true)
		for _, alias := range item.Aliases {
			add(alias.Path, alias.GitHub, alias.R2, false)
		}
	}
	return rows
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
	rows := urlMappingRows(items)
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	header := []string{"asset_id", "old_path", "new_path", "old_github_url", "new_github_url", "old_r2_url", "new_r2_url", "size_bytes", "status", "error", "sha256"}
	if err := w.Write(header); err != nil {
		return "", "", err
	}
	for _, item := range rows {
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
	data, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return "", "", err
	}
	if err := atomicfile.Write(ctx, jsonPath, append(data, '\n'), 0o600); err != nil {
		return "", "", err
	}
	return csvPath, jsonPath, nil
}
