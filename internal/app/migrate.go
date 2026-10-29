package app

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/delivery"
	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/index"
	"github.com/Eurekaimer/TOUKO/internal/repository"
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
	Items      []MigrateItem         `json:"items"`
	Cloudflare *CloudflareResult     `json:"cloudflare,omitempty"`
}

// MigratePaths rewrites every asset to objects/<namespace>/<asset-id>.<ext> in
// one commit. Old objects stay in the tree so old URLs keep working until a
// later prune run deletes them; nothing is deleted in the same pass that
// creates the new paths.
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
	if err := mutable(st); err != nil {
		return MigrateResult{}, err
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
	// A repository without a configured location is still migratable; it simply
	// has no public URLs to record.
	location, _ := s.Location(ctx)
	r2Base := ""
	if cfg, cfgErr := s.CloudflareConfig(ctx); cfgErr == nil && cfg.Enabled {
		r2Base = cfg.PublicBaseURL
	}
	result := MigrateResult{Head: head, DryRun: dryRun, Prune: prune, Items: []MigrateItem{}}

	now := time.Now().UTC()
	helpers := urlBuilder{location: location, r2Base: r2Base}
	ids := make([]domain.AssetID, 0, len(base.Assets))
	for id := range base.Assets {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	changes := make([]domain.Change, 0, len(ids))
	updates := make(map[domain.AssetID]domain.Asset, len(ids))
	byNewPath := make(map[string]domain.Asset, len(ids))
	for _, id := range ids {
		asset := base.Assets[id]
		newPath := repository.AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext)
		item := MigrateItem{
			AssetID: asset.ID.String(), OldPath: repository.ObjectPath(asset), NewPath: newPath,
			SizeBytes: asset.Object.Size, Status: "unchanged",
		}
		item.OldGitHub, item.NewGitHub = helpers.github(item.OldPath), helpers.github(newPath)
		item.OldR2, item.NewR2 = helpers.r2(item.OldPath), helpers.r2(newPath)
		if item.OldPath != newPath {
			updated := asset
			updated.Path = newPath
			updated.UpdatedAt = now
			changes = append(changes, domain.Change{Kind: domain.Update, AssetID: id, Asset: &updated})
			updates[id] = updated
			byNewPath[newPath] = updated
			item.Status = "planned"
		}
		result.Items = append(result.Items, item)
	}
	var deletions []string
	if prune {
		referenced := make(map[string]bool, len(base.Assets))
		for _, asset := range base.Assets {
			referenced[repository.ObjectPath(asset)] = true
		}
		for _, entry := range base.Entries {
			if entry.Type == "blob" && strings.HasPrefix(entry.Path, "objects/") && !referenced[entry.Path] {
				deletions = append(deletions, entry.Path)
			}
		}
		if len(deletions) == 0 && len(changes) == 0 {
			return result, nil
		}
		// A prune run only removes objects unreferenced by any asset; it never
		// rewrites metadata.
		if len(changes) == 0 && len(deletions) > 0 {
			return s.pruneObjects(ctx, r, st, base, result, deletions, dryRun)
		}
	}
	if len(changes) == 0 {
		return result, nil
	}
	if dryRun {
		for i := range result.Items {
			if result.Items[i].Status == "planned" {
				result.Items[i].Status = "would_migrate"
				result.Migrated++
			} else if result.Pruned == 0 {
				result.Unchanged++
			}
		}
		return result, nil
	}
	plan, err := repository.PlanCommit(base, domain.ChangeSet{BaseHead: head, Changes: changes})
	if err != nil {
		return MigrateResult{}, err
	}
	// Keep the old placements: they are the URLs currently in use.
	plan.Deletions = filterLegacyDeletions(plan.Deletions)
	if len(deletions) > 0 {
		plan.Deletions = append(plan.Deletions, deletions...)
	}
	blobs := make(map[string]repository.BlobID, len(plan.NewObjects)+len(plan.MetadataWrites))
	treeChanges := make([]repository.TreeChange, 0, len(plan.NewObjects)+len(plan.MetadataWrites)+len(plan.Deletions))
	for _, p := range plan.NewObjects {
		source, ok := byNewPath[p.Path]
		if !ok {
			return MigrateResult{}, fmt.Errorf("migration source for %s is unknown", p.Path)
		}
		oldPath := repository.ObjectPath(base.Assets[source.ID])
		oldEntry, ok := entryAt(base.Entries, oldPath)
		if !ok {
			return MigrateResult{}, fmt.Errorf("migration source for %s is missing", oldPath)
		}
		body, err := r.OpenBlob(ctx, repository.BlobID(oldEntry.SHA))
		if err != nil {
			return MigrateResult{}, err
		}
		blob, err := r.CreateBlob(ctx, body, source.Object.Size)
		closeErr := body.Close()
		if err != nil {
			return MigrateResult{}, err
		}
		if closeErr != nil {
			return MigrateResult{}, closeErr
		}
		blobs[p.Path] = blob
		treeChanges = append(treeChanges, repository.TreeChange{Path: p.Path, Blob: &blob})
	}
	for _, write := range plan.MetadataWrites {
		blob, err := r.CreateBlob(ctx, bytes.NewReader(write.Data), int64(len(write.Data)))
		if err != nil {
			return MigrateResult{}, err
		}
		blobs[write.Path] = blob
		treeChanges = append(treeChanges, repository.TreeChange{Path: write.Path, Blob: &blob})
	}
	for _, path := range plan.Deletions {
		treeChanges = append(treeChanges, repository.TreeChange{Path: path})
	}
	tree, err := r.CreateTree(ctx, base.Tree, treeChanges)
	if err != nil {
		return MigrateResult{}, err
	}
	candidate, err := r.CreateCommit(ctx, tree, head, "migrate: use asset ids in object paths")
	if err != nil {
		return MigrateResult{}, err
	}
	snap, err := plannedSnapshot(base, plan, blobs, candidate, tree)
	if err != nil {
		return MigrateResult{}, err
	}
	st.Publication = &index.Publication{BaseHead: head, CandidateHead: candidate, CandidateTree: string(tree)}
	if err = s.Store.SavePublication(ctx, st.Publication); err != nil {
		return MigrateResult{}, err
	}
	published, err := s.publish(ctx, r, st, snap)
	if err != nil {
		return result, err
	}
	result.Head = published.Head
	for i := range result.Items {
		item := &result.Items[i]
		if item.Status != "planned" {
			result.Unchanged++
			continue
		}
		updated := updates[mustAssetID(item.AssetID)]
		blob, ok := blobs[item.NewPath]
		if !ok {
			item.Status, item.Error = "failed", "new object was not created"
			result.Failed++
			continue
		}
		if err := repository.VerifyObject(ctx, r, blob, updated.Object); err != nil {
			item.Status, item.Error = "failed", err.Error()
			result.Failed++
			continue
		}
		item.Status = "migrated"
		result.Migrated++
	}
	if result.Failed > 0 {
		return result, fmt.Errorf("%d migrated objects could not be verified; old objects were kept", result.Failed)
	}
	if r2Base != "" {
		mirror, syncErr := s.CloudflareSync(ctx, r, CloudflareOptions{})
		result.Cloudflare = &mirror
		if syncErr != nil {
			return result, syncErr
		}
	}
	return result, nil
}

// pruneObjects removes old object paths from the tree once every asset uses the
// asset-id layout and the new objects are verified. It never touches metadata.
func (s *Service) pruneObjects(ctx context.Context, r repository.Git, st index.State, base repository.Snapshot, result MigrateResult, deletions []string, dryRun bool) (MigrateResult, error) {
	for i := range result.Items {
		result.Items[i].Status = "unchanged"
		result.Unchanged++
	}
	if dryRun {
		result.Pruned = len(deletions)
		return result, nil
	}
	changes := make([]repository.TreeChange, 0, len(deletions))
	for _, path := range deletions {
		changes = append(changes, repository.TreeChange{Path: path})
	}
	tree, err := r.CreateTree(ctx, base.Tree, changes)
	if err != nil {
		return MigrateResult{}, err
	}
	head := base.Head
	candidate, err := r.CreateCommit(ctx, tree, head, "migrate: remove legacy content-addressed objects")
	if err != nil {
		return MigrateResult{}, err
	}
	plan := repository.CommitPlan{FinalAssets: base.Assets, Deletions: deletions}
	snap, err := plannedSnapshot(base, plan, nil, candidate, tree)
	if err != nil {
		return MigrateResult{}, err
	}
	st.Publication = &index.Publication{BaseHead: head, CandidateHead: candidate, CandidateTree: string(tree)}
	if err = s.Store.SavePublication(ctx, st.Publication); err != nil {
		return MigrateResult{}, err
	}
	published, err := s.publish(ctx, r, st, snap)
	if err != nil {
		return result, err
	}
	result.Head = published.Head
	result.Pruned = len(deletions)
	return result, nil
}

func entryAt(entries []repository.GitEntry, path string) (repository.GitEntry, bool) {
	for _, entry := range entries {
		if entry.Path == path {
			return entry, true
		}
	}
	return repository.GitEntry{}, false
}

func filterLegacyDeletions(paths []string) []string {
	out := paths[:0]
	for _, path := range paths {
		if !strings.HasPrefix(path, "objects/") {
			out = append(out, path)
		}
	}
	return out
}

func mustAssetID(value string) domain.AssetID {
	id, _ := domain.ParseAssetID(value)
	return id
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

// WriteURLMapping writes migration/url-mapping.csv and .json under dir.
func WriteURLMapping(dir string, items []MigrateItem) (string, string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	csvPath := filepath.Join(dir, "url-mapping.csv")
	jsonPath := filepath.Join(dir, "url-mapping.json")
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	header := []string{"asset_id", "old_path", "new_path", "old_github_url", "new_github_url", "old_r2_url", "new_r2_url", "size_bytes", "status", "error"}
	if err := w.Write(header); err != nil {
		return "", "", err
	}
	for _, item := range items {
		row := []string{item.AssetID, item.OldPath, item.NewPath, item.OldGitHub, item.NewGitHub, item.OldR2, item.NewR2, strconv.FormatInt(item.SizeBytes, 10), item.Status, item.Error}
		if err := w.Write(row); err != nil {
			return "", "", err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(csvPath, buf.Bytes(), 0o600); err != nil {
		return "", "", err
	}
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(jsonPath, append(data, '\n'), 0o600); err != nil {
		return "", "", err
	}
	return csvPath, jsonPath, nil
}
