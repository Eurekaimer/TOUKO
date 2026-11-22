package repository

import "github.com/Eurekaimer/TOUKO/internal/domain"

// ImageBlobs records immutable Git IDs without reading image payloads.
func (s Snapshot) ImageBlobs() map[domain.AssetID]BlobID {
	if len(s.Assets) == 0 {
		return nil
	}
	entries := make(map[string]GitEntry, len(s.Entries))
	for _, entry := range s.Entries {
		if entry.Type == "blob" && (entry.Mode == "100644" || entry.Mode == "100755") {
			entries[entry.Path] = entry
		}
	}
	blobs := make(map[domain.AssetID]BlobID, len(s.Assets))
	for id, asset := range s.Assets {
		entry := entries[ObjectPath(asset)]
		if entry.SHA != "" && entry.Size == asset.Object.Size {
			blobs[id] = BlobID(entry.SHA)
		}
	}
	return blobs
}
