package index

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/Eurekaimer/TOUKO/internal/domain"
)

const (
	mirrorLedgerVersion  = 1
	mirrorDirectoryName  = "cloudflare"
	mirrorLedgerMaxBytes = 64 << 20
	mirrorDirectoryPerm  = 0o700
)

// MirrorLedger is the optional durable receipt for one Cloudflare R2 target.
// It lives outside workspace.json and the SQLite cache so a mirror failure can
// never affect GitHub publication or gallery state.
type MirrorLedger struct {
	Version      int                 `json:"version"`
	TargetID     string              `json:"target_id"`
	Endpoint     string              `json:"endpoint"`
	Bucket       string              `json:"bucket"`
	RepositoryID domain.RepositoryID `json:"repository_id"`
	SourceID     string              `json:"source_id"`
	Entries      []MirrorEntry       `json:"entries"`
}

// MirrorEntry records the last expected object for one key and the state of its
// last mirror attempt. Object is always the last known ContentObject, including
// for deletion tombstones.
type MirrorEntry struct {
	Key       string               `json:"key"`
	Object    domain.ContentObject `json:"object"`
	ETag      string               `json:"etag"`
	State     string               `json:"state"`
	ErrorCode string               `json:"error_code,omitempty"`
}

func mirrorCorrupt(format string, args ...any) error {
	return corrupt(format, args...)
}

func validMirrorState(value string) bool {
	switch value {
	case "pending", "synced", "failed", "delete_pending", "delete_failed", "deleted":
		return true
	default:
		return false
	}
}

// LoadMirror reads the ledger for targetID from <dir>/cloudflare. A missing
// ledger is not an error: it returns a zero ledger so callers can distinguish
// "no receipt" (Version 0) from a valid empty ledger (Version 1). It never
// creates the directory and never repairs a damaged file.
func LoadMirror(ctx context.Context, dir, targetID string) (MirrorLedger, error) {
	if err := ctx.Err(); err != nil {
		return MirrorLedger{}, err
	}
	if !validMirrorHash(targetID) {
		return MirrorLedger{}, mirrorCorrupt("invalid mirror target id %q", targetID)
	}
	f, err := os.Open(mirrorLedgerPath(dir, targetID))
	if errors.Is(err, fs.ErrNotExist) {
		return MirrorLedger{}, nil
	}
	if err != nil {
		return MirrorLedger{}, fmt.Errorf("read mirror ledger: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, mirrorLedgerMaxBytes+1))
	if err != nil {
		return MirrorLedger{}, fmt.Errorf("read mirror ledger: %w", err)
	}
	if len(data) > mirrorLedgerMaxBytes {
		return MirrorLedger{}, mirrorCorrupt("mirror ledger exceeds %d bytes", mirrorLedgerMaxBytes)
	}
	if err := ctx.Err(); err != nil {
		return MirrorLedger{}, err
	}
	var ledger MirrorLedger
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ledger); err != nil {
		return MirrorLedger{}, mirrorCorrupt("invalid mirror ledger: %v", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return MirrorLedger{}, mirrorCorrupt("trailing data in mirror ledger")
	}
	if err := validateMirrorLedger(ledger); err != nil {
		return MirrorLedger{}, err
	}
	if ledger.TargetID != targetID {
		return MirrorLedger{}, mirrorCorrupt("mirror ledger target id differs from requested target")
	}
	if ledger.Entries == nil {
		ledger.Entries = []MirrorEntry{}
	}
	sortMirrorEntries(ledger.Entries)
	return ledger, nil
}

// SaveMirror durably replaces the ledger for ledger.TargetID. It validates the
// complete ledger, including every saved entry, before creating a temporary
// file, so invalid input cannot damage an existing receipt. The caller decides
// whether a damaged existing ledger may be replaced.
func SaveMirror(ctx context.Context, dir string, ledger MirrorLedger) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateMirrorLedger(ledger); err != nil {
		return err
	}
	next := ledger
	next.Entries = append([]MirrorEntry{}, ledger.Entries...)
	sortMirrorEntries(next.Entries)
	data, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("encode mirror ledger: %w", err)
	}
	if len(data)+1 > mirrorLedgerMaxBytes {
		return mirrorCorrupt("mirror ledger exceeds %d bytes", mirrorLedgerMaxBytes)
	}
	targetDir := filepath.Join(dir, mirrorDirectoryName)
	if err := os.MkdirAll(targetDir, mirrorDirectoryPerm); err != nil {
		return fmt.Errorf("create mirror ledger directory: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.CreateTemp(targetDir, ".mirror-*")
	if err != nil {
		return fmt.Errorf("create durable mirror ledger: %w", err)
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(append(data, '\n')); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return fmt.Errorf("write durable mirror ledger: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("write durable mirror ledger: %w", closeErr)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err = durableRename(name, mirrorLedgerPath(dir, ledger.TargetID))
	if err != nil {
		return fmt.Errorf("replace mirror ledger: %w", err)
	}
	return nil
}

func mirrorLedgerPath(dir, targetID string) string {
	return filepath.Join(dir, mirrorDirectoryName, targetID+".json")
}

func sortMirrorEntries(entries []MirrorEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
}

func validateMirrorLedger(ledger MirrorLedger) error {
	if ledger.Version != mirrorLedgerVersion {
		return mirrorCorrupt("unsupported mirror ledger version %d", ledger.Version)
	}
	if !validMirrorHash(ledger.TargetID) {
		return mirrorCorrupt("invalid mirror target id %q", ledger.TargetID)
	}
	targetID, err := mirrorTargetID(ledger.Endpoint, ledger.Bucket)
	if err != nil {
		return mirrorCorrupt("invalid mirror target: %v", err)
	}
	if targetID != ledger.TargetID {
		return mirrorCorrupt("mirror target id does not match endpoint and bucket")
	}
	if ledger.RepositoryID == (domain.RepositoryID{}) {
		return mirrorCorrupt("mirror ledger has no repository identity")
	}
	if !validMirrorHash(ledger.SourceID) {
		return mirrorCorrupt("invalid mirror source id")
	}
	seen := make(map[string]struct{}, len(ledger.Entries))
	for i := range ledger.Entries {
		entry := &ledger.Entries[i]
		if err := validateMirrorEntry(entry); err != nil {
			return err
		}
		if _, duplicate := seen[entry.Key]; duplicate {
			return mirrorCorrupt("duplicate mirror ledger key %q", entry.Key)
		}
		seen[entry.Key] = struct{}{}
	}
	return nil
}

func validateMirrorEntry(entry *MirrorEntry) error {
	if err := domain.ValidateRepositoryPath(entry.Key); err != nil {
		return mirrorCorrupt("invalid mirror entry key: %v", err)
	}
	if entry.Object.Hash == (domain.ObjectHash{}) {
		return mirrorCorrupt("mirror entry %q has no object hash", entry.Key)
	}
	if err := entry.Object.Validate(); err != nil {
		return mirrorCorrupt("invalid mirror entry object for %q: %v", entry.Key, err)
	}
	if !validMirrorState(entry.State) {
		return mirrorCorrupt("invalid mirror entry state %q for %q", entry.State, entry.Key)
	}
	if entry.ETag != "" && !validMirrorETag(entry.ETag) {
		return mirrorCorrupt("invalid mirror entry etag for %q", entry.Key)
	}
	if entry.State == "synced" && entry.ETag == "" {
		return mirrorCorrupt("synced mirror entry %q has no etag", entry.Key)
	}
	if entry.ErrorCode != "" {
		if !validMirrorErrorCode(entry.ErrorCode) {
			return mirrorCorrupt("invalid mirror entry error code for %q", entry.Key)
		}
		if entry.State == "synced" || entry.State == "deleted" {
			return mirrorCorrupt("mirror entry %q in state %s must not carry an error code", entry.Key, entry.State)
		}
	}
	return nil
}

// mirrorTargetID reproduces r2.Config.TargetID without making the index
// package depend on the optional Cloudflare adapter. The endpoint is normalized
// to lowercase scheme and host with no root path.
func mirrorTargetID(endpoint, bucket string) (string, error) {
	normalized, err := normalizeMirrorEndpoint(endpoint)
	if err != nil {
		return "", err
	}
	if err := validateMirrorBucket(bucket); err != nil {
		return "", err
	}
	raw, err := json.Marshal([]string{normalized, bucket})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func normalizeMirrorEndpoint(raw string) (string, error) {
	invalid := errors.New("invalid Cloudflare R2 endpoint")
	if raw == "" || strings.ContainsRune(raw, '#') {
		return "", invalid
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", invalid
	}
	if !strings.EqualFold(u.Scheme, "https") || u.Opaque != "" || u.Host == "" || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" ||
		strings.ContainsAny(u.Host, " \t\r\n\\") {
		return "", invalid
	}
	if u.Path != "" && u.Path != "/" {
		return "", invalid
	}
	return "https://" + strings.ToLower(u.Host), nil
}

func validateMirrorBucket(bucket string) error {
	invalid := errors.New("invalid Cloudflare R2 bucket")
	if bucket == "" {
		return invalid
	}
	for _, r := range bucket {
		if r == '/' || r == '\\' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return invalid
		}
	}
	return nil
}

// validMirrorHash accepts the lowercase SHA-256 hex form used by both target
// and source identities.
func validMirrorHash(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// validMirrorETag matches the normalized ETag shape accepted by the R2
// adapter's conditional writes.
func validMirrorETag(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e || r == '"' || r == '\\' {
			return false
		}
	}
	return true
}

// Only the adapter and application failure codes may enter durable state.
func validMirrorErrorCode(value string) bool {
	switch value {
	case "credentials_missing", "configuration_invalid", "bucket_unavailable",
		"access_denied", "not_found", "precondition_failed", "network", "timeout",
		"canceled", "object_conflict", "mirror_state_invalid",
		"mirror_state_write_failed", "source_changed", "source_unavailable",
		"hash_mismatch", "deferred_put_failure", "not_published", "not_synced",
		"local_repository":
		return true
	default:
		return false
	}
}
