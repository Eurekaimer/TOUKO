package domain

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

type AssetID [16]byte
type RepositoryID [16]byte
type ObjectHash [32]byte
type RepositoryHead string

func NewAssetID() (AssetID, error) { id, err := uuid.NewRandom(); return AssetID(id), err }
func NewRepositoryID() (RepositoryID, error) {
	id, err := uuid.NewRandom()
	return RepositoryID(id), err
}
func (id AssetID) String() string      { return uuid.UUID(id).String() }
func (id RepositoryID) String() string { return uuid.UUID(id).String() }
func parseUUID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil || id.String() != s || id == uuid.Nil {
		return uuid.Nil, fmt.Errorf("invalid lowercase UUID %q", s)
	}
	return id, nil
}
func ParseAssetID(s string) (AssetID, error) { id, err := parseUUID(s); return AssetID(id), err }
func ParseRepositoryID(s string) (RepositoryID, error) {
	id, err := parseUUID(s)
	return RepositoryID(id), err
}
func (id AssetID) MarshalText() ([]byte, error)      { return []byte(id.String()), nil }
func (id RepositoryID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }
func (id *AssetID) UnmarshalText(b []byte) error {
	v, err := ParseAssetID(string(b))
	if err == nil {
		*id = v
	}
	return err
}
func (id *RepositoryID) UnmarshalText(b []byte) error {
	v, err := ParseRepositoryID(string(b))
	if err == nil {
		*id = v
	}
	return err
}
func (h ObjectHash) String() string               { return hex.EncodeToString(h[:]) }
func (h ObjectHash) MarshalText() ([]byte, error) { return []byte(h.String()), nil }
func (h *ObjectHash) UnmarshalText(b []byte) error {
	if len(b) != 64 || strings.ToLower(string(b)) != string(b) {
		return fmt.Errorf("object hash must be 64 lowercase hex characters")
	}
	var v ObjectHash
	if _, err := hex.Decode(v[:], b); err != nil {
		return err
	}
	*h = v
	return nil
}

func ResolveAsset(assets map[AssetID]Asset, selector string) (AssetID, error) {
	if len(selector) < 8 || len(selector) > 36 {
		return AssetID{}, fmt.Errorf("%w: %q (UUID or prefix of at least 8 characters required)", ErrAssetNotFound, selector)
	}
	var match AssetID
	found := false
	for id := range assets {
		if strings.HasPrefix(id.String(), selector) {
			if found {
				return AssetID{}, fmt.Errorf("%w: %s", ErrAmbiguousSelector, selector)
			}
			match = id
			found = true
		}
	}
	if !found {
		return AssetID{}, fmt.Errorf("%w: %s", ErrAssetNotFound, selector)
	}
	return match, nil
}
