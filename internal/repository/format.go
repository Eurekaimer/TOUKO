package repository

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/domain"
)

const MaxMetadataBytes = 1 << 20
const MaxManifestBytes = 64 << 10

func corrupt(err error) error { return fmt.Errorf("%w: %v", domain.ErrRepositoryCorrupt, err) }
func decode(data []byte, max int, v any) error {
	if len(data) > max {
		return fmt.Errorf("metadata exceeds %d bytes", max)
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	return nil
}
func encode(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
func ValidateManifest(m domain.Repository) error {
	if (m.FormatVersion != 1 && m.FormatVersion != 2) || m.HashAlgorithm != "sha256" || m.ID == (domain.RepositoryID{}) {
		return corrupt(fmt.Errorf("unsupported or invalid repository manifest (version %d)", m.FormatVersion))
	}
	return nil
}
func EncodeManifest(m domain.Repository) ([]byte, error) {
	if err := ValidateManifest(m); err != nil {
		return nil, err
	}
	return encode(m)
}
func DecodeManifest(data []byte) (domain.Repository, error) {
	var m domain.Repository
	if err := decode(data, MaxManifestBytes, &m); err != nil {
		return m, corrupt(err)
	}
	return m, ValidateManifest(m)
}
func EncodeAsset(a domain.Asset) ([]byte, error) {
	if err := a.Validate(); err != nil {
		return nil, corrupt(err)
	}
	a.CreatedAt = a.CreatedAt.In(time.UTC)
	a.UpdatedAt = a.UpdatedAt.In(time.UTC)
	if a.Tags == nil {
		a.Tags = []string{}
	}
	if a.Collections == nil {
		a.Collections = []string{}
	}
	b, err := encode(a)
	if err == nil && len(b) > MaxMetadataBytes {
		return nil, corrupt(fmt.Errorf("metadata too large"))
	}
	return b, err
}
func DecodeAsset(data []byte, filename string) (domain.Asset, error) {
	var a domain.Asset
	if err := decode(data, MaxMetadataBytes, &a); err != nil {
		return a, corrupt(err)
	}
	if err := a.Validate(); err != nil {
		return a, corrupt(err)
	}
	if filename != a.ID.String()+".json" {
		return a, corrupt(fmt.Errorf("metadata filename differs from asset ID"))
	}
	if a.Tags == nil {
		a.Tags = []string{}
	}
	if a.Collections == nil {
		a.Collections = []string{}
	}
	return a, nil
}
