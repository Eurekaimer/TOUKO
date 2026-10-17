package repository

import "fmt"

// BlobBytes returns the total size of every Git blob in entries. Tree entries
// and submodule commits hold no blob bytes; every blob, including files that
// TOUKO does not manage, is counted. A missing size is an error rather than
// zero so an incomplete tree can never be mistaken for an empty repository.
func BlobBytes(entries []GitEntry) (int64, error) {
	_, total, err := blobIndex(entries)
	return total, err
}

// ProjectedBlobBytes returns the blob bytes a commit plan would leave in the
// tree. It starts from the complete base tree, then applies metadata
// replacements and deletions and adds each new object once. Reused objects are
// already present in the base and are never added again.
func ProjectedBlobBytes(base Snapshot, plan CommitPlan) (int64, error) {
	entries, total, err := blobIndex(base.Entries)
	if err != nil {
		return 0, err
	}
	touched := make(map[string]bool, len(plan.Deletions)+len(plan.MetadataWrites)+len(plan.NewObjects)+len(plan.ReusedObjects))
	for _, path := range plan.Deletions {
		entry, ok := entries[path]
		if !ok {
			return 0, corrupt(fmt.Errorf("deletion is absent from the base tree: %s", path))
		}
		if entry.Type != "blob" || entry.Size < 0 {
			return 0, corrupt(fmt.Errorf("deletion is not a sized blob: %s", path))
		}
		if touched[path] {
			return 0, corrupt(fmt.Errorf("tree path is applied more than once: %s", path))
		}
		touched[path] = true
		total -= entry.Size
	}
	for _, write := range plan.MetadataWrites {
		if touched[write.Path] {
			return 0, corrupt(fmt.Errorf("tree path is applied more than once: %s", write.Path))
		}
		touched[write.Path] = true
		if entry, ok := entries[write.Path]; ok {
			if entry.Type != "blob" || entry.Size < 0 {
				return 0, corrupt(fmt.Errorf("metadata replacement is not a sized blob: %s", write.Path))
			}
			total -= entry.Size
		}
		total += int64(len(write.Data))
	}
	for _, placement := range plan.NewObjects {
		if touched[placement.Path] {
			return 0, corrupt(fmt.Errorf("tree path is applied more than once: %s", placement.Path))
		}
		if _, ok := entries[placement.Path]; ok {
			return 0, corrupt(fmt.Errorf("new object already exists in the base tree: %s", placement.Path))
		}
		if err := placement.Object.Validate(); err != nil {
			return 0, corrupt(err)
		}
		touched[placement.Path] = true
		total += placement.Object.Size
	}
	for _, placement := range plan.ReusedObjects {
		entry, ok := entries[placement.Path]
		if !ok {
			return 0, corrupt(fmt.Errorf("reused object is absent from the base tree: %s", placement.Path))
		}
		if entry.Type != "blob" || entry.Size < 0 || entry.Size != placement.Object.Size {
			return 0, corrupt(fmt.Errorf("reused object disagrees with the base tree: %s", placement.Path))
		}
		if touched[placement.Path] {
			return 0, corrupt(fmt.Errorf("tree path is applied more than once: %s", placement.Path))
		}
		touched[placement.Path] = true
	}
	if total < 0 {
		return 0, corrupt(fmt.Errorf("projected blob bytes are negative"))
	}
	return total, nil
}

func blobIndex(tree []GitEntry) (map[string]GitEntry, int64, error) {
	entries := make(map[string]GitEntry, len(tree))
	var total int64
	for _, entry := range tree {
		if entry.Path == "" {
			return nil, 0, corrupt(fmt.Errorf("tree entry has an empty path"))
		}
		if _, ok := entries[entry.Path]; ok {
			return nil, 0, corrupt(fmt.Errorf("duplicate tree path %s", entry.Path))
		}
		entries[entry.Path] = entry
		if entry.Type != "blob" {
			continue
		}
		if entry.Size < 0 {
			return nil, 0, corrupt(fmt.Errorf("missing blob size for %s", entry.Path))
		}
		total += entry.Size
	}
	return entries, total, nil
}
