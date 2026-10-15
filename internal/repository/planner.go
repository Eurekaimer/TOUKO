package repository

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/Eurekaimer/TOUKO/internal/domain"
)

type MetadataWrite struct {
	Path string
	Data []byte
}
type ObjectPlacement struct {
	Path      string
	Namespace domain.Namespace
	Object    domain.ContentObject
	Blob      BlobID
}
type CommitPlan struct {
	ExpectedHead   domain.RepositoryHead
	MetadataWrites []MetadataWrite
	NewObjects     []ObjectPlacement
	ReusedObjects  []ObjectPlacement
	Deletions      []string
	FinalAssets    map[domain.AssetID]domain.Asset
}

func (p CommitPlan) Empty() bool {
	return len(p.MetadataWrites) == 0 && len(p.NewObjects) == 0 && len(p.Deletions) == 0
}
func placements(assets map[domain.AssetID]domain.Asset) (map[string]ObjectPlacement, error) {
	out := make(map[string]ObjectPlacement)
	descriptions := make(map[domain.ObjectHash]domain.ContentObject)
	for id, a := range assets {
		if id != a.ID {
			return nil, corrupt(fmt.Errorf("asset map ID mismatch"))
		}
		if err := a.Validate(); err != nil {
			return nil, corrupt(err)
		}
		if old, ok := descriptions[a.Object.Hash]; ok && old != a.Object {
			return nil, corrupt(fmt.Errorf("contradictory object description %s", a.Object.Hash))
		}
		descriptions[a.Object.Hash] = a.Object
		p := BuildObjectPath(a.Namespace, a.Object)
		out[p] = ObjectPlacement{Path: p, Namespace: a.Namespace, Object: a.Object}
	}
	return out, nil
}
func PlanCommit(base Snapshot, changes domain.ChangeSet) (CommitPlan, error) {
	p := CommitPlan{ExpectedHead: base.Head, FinalAssets: make(map[domain.AssetID]domain.Asset, len(base.Assets)), MetadataWrites: []MetadataWrite{}, NewObjects: []ObjectPlacement{}, ReusedObjects: []ObjectPlacement{}, Deletions: []string{}}
	if base.Head != changes.BaseHead {
		return CommitPlan{}, &domain.ConflictError{Base: changes.BaseHead, Remote: base.Head, Reason: "head_changed"}
	}
	if err := ValidateManifest(base.Manifest); err != nil {
		return CommitPlan{}, err
	}
	for id, a := range base.Assets {
		p.FinalAssets[id] = a
	}
	seen := make(map[domain.AssetID]bool)
	for _, c := range changes.Changes {
		if c.AssetID == (domain.AssetID{}) || seen[c.AssetID] {
			return CommitPlan{}, fmt.Errorf("%w: change must be normalized to one operation per Asset", domain.ErrConflict)
		}
		seen[c.AssetID] = true
		_, exists := base.Assets[c.AssetID]
		switch c.Kind {
		case domain.Add, domain.Update:
			if c.Asset == nil || c.Asset.ID != c.AssetID {
				return CommitPlan{}, fmt.Errorf("%w: add/update requires complete matching metadata", domain.ErrConflict)
			}
			if c.Kind == domain.Add && exists {
				return CommitPlan{}, fmt.Errorf("%w: asset already exists: %s", domain.ErrConflict, c.AssetID)
			}
			if c.Kind == domain.Update && !exists {
				return CommitPlan{}, fmt.Errorf("%w: %s", domain.ErrAssetNotFound, c.AssetID)
			}
			if err := c.Asset.Validate(); err != nil {
				return CommitPlan{}, fmt.Errorf("%w: %v", domain.ErrConflict, err)
			}
			p.FinalAssets[c.AssetID] = *c.Asset
		case domain.Delete:
			if c.Asset != nil {
				return CommitPlan{}, fmt.Errorf("%w: delete must not carry metadata", domain.ErrConflict)
			}
			if !exists {
				return CommitPlan{}, fmt.Errorf("%w: %s", domain.ErrAssetNotFound, c.AssetID)
			}
			delete(p.FinalAssets, c.AssetID)
		default:
			return CommitPlan{}, fmt.Errorf("%w: invalid change kind %q", domain.ErrConflict, c.Kind)
		}
	}
	before, err := placements(base.Assets)
	if err != nil {
		return CommitPlan{}, err
	}
	after, err := placements(p.FinalAssets)
	if err != nil {
		return CommitPlan{}, err
	}
	entries := make(map[string]GitEntry, len(base.Entries))
	for _, entry := range base.Entries {
		entries[entry.Path] = entry
	}
	touched := make(map[string]bool)
	for id, a := range p.FinalAssets {
		encoded, err := EncodeAsset(a)
		if err != nil {
			return CommitPlan{}, err
		}
		if old, ok := base.Assets[id]; ok {
			previous, err := EncodeAsset(old)
			if err != nil {
				return CommitPlan{}, err
			}
			if bytes.Equal(encoded, previous) {
				continue
			}
		}
		p.MetadataWrites = append(p.MetadataWrites, MetadataWrite{Path: AssetPath(id), Data: encoded})
		touched[BuildObjectPath(a.Namespace, a.Object)] = true
	}
	for id := range base.Assets {
		if _, ok := p.FinalAssets[id]; !ok {
			p.Deletions = append(p.Deletions, AssetPath(id))
		}
	}
	for path, placement := range before {
		if _, ok := after[path]; !ok {
			entry, exists := entries[path]
			if !exists || entry.Type != "blob" || entry.Mode != "100644" || entry.Size != placement.Object.Size {
				return CommitPlan{}, corrupt(fmt.Errorf("missing base placement %s", path))
			}
			p.Deletions = append(p.Deletions, path)
		}
	}
	for path, placement := range after {
		entry, exists := entries[path]
		if !exists {
			if _, wasReferenced := before[path]; wasReferenced {
				return CommitPlan{}, corrupt(fmt.Errorf("missing base placement %s", path))
			}
			p.NewObjects = append(p.NewObjects, placement)
			continue
		}
		if entry.Type != "blob" || entry.Mode != "100644" || entry.Size != placement.Object.Size {
			return CommitPlan{}, corrupt(fmt.Errorf("occupied canonical placement %s", path))
		}
		if touched[path] {
			placement.Blob = BlobID(entry.SHA)
			p.ReusedObjects = append(p.ReusedObjects, placement)
		}
	}
	sort.Slice(p.MetadataWrites, func(i, j int) bool { return p.MetadataWrites[i].Path < p.MetadataWrites[j].Path })
	sort.Slice(p.NewObjects, func(i, j int) bool { return p.NewObjects[i].Path < p.NewObjects[j].Path })
	sort.Slice(p.ReusedObjects, func(i, j int) bool { return p.ReusedObjects[i].Path < p.ReusedObjects[j].Path })
	sort.Strings(p.Deletions)
	return p, nil
}
