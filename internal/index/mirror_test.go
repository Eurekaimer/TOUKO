package index

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/r2"
)

const (
	testMirrorSourceID   = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testMirrorETag       = "0123456789abcdef0123456789abcdef"
	maxMirrorLedgerBytes = 64 << 20
)

func testMirrorConfig() r2.Config {
	return r2.Config{
		Enabled:       true,
		Endpoint:      "https://ACCOUNT.R2.CloudflareStorage.com/",
		Bucket:        "touko-images",
		PublicBaseURL: "https://img.eurekaimer.icu",
	}
}

func sampleMirrorLedger(t *testing.T) MirrorLedger {
	t.Helper()
	cfg := testMirrorConfig()
	rid, err := domain.ParseRepositoryID("22222222-2222-4222-8222-222222222222")
	if err != nil {
		t.Fatal(err)
	}
	object := domain.ContentObject{
		Hash:   domain.ObjectHash{0xde, 0xad, 0xbe, 0xef},
		MIME:   "image/webp",
		Ext:    "webp",
		Size:   4096,
		Width:  800,
		Height: 600,
	}
	return MirrorLedger{
		Version:      1,
		TargetID:     cfg.TargetID(),
		Endpoint:     cfg.Endpoint,
		Bucket:       cfg.Bucket,
		RepositoryID: rid,
		SourceID:     testMirrorSourceID,
		Entries: []MirrorEntry{
			{Key: "z-last/touko.webp", Object: object, ETag: testMirrorETag, State: "synced"},
			{Key: "images/零 空格.webp", Object: object, ETag: testMirrorETag, State: "failed", ErrorCode: "network"},
			{Key: "objects/old.webp", Object: object, ETag: testMirrorETag, State: "delete_pending", ErrorCode: "deferred_put_failure"},
		},
	}
}

func cloneMirrorLedger(l MirrorLedger) MirrorLedger {
	l.Entries = append([]MirrorEntry(nil), l.Entries...)
	return l
}

func sortedMirrorLedger(l MirrorLedger) MirrorLedger {
	l = cloneMirrorLedger(l)
	sort.Slice(l.Entries, func(i, j int) bool { return l.Entries[i].Key < l.Entries[j].Key })
	return l
}

func testMirrorLedgerPath(stateDir, targetID string) string {
	return filepath.Join(stateDir, "cloudflare", targetID+".json")
}

func writeMirrorRaw(t *testing.T, stateDir, targetID string, data []byte) string {
	t.Helper()
	dir := filepath.Join(stateDir, "cloudflare")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, targetID+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func requireMirrorCorrupt(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a corruption error")
	}
	if !errors.Is(err, domain.ErrRepositoryCorrupt) {
		t.Fatalf("error %v does not wrap domain.ErrRepositoryCorrupt", err)
	}
}

func TestMirrorLedgerRoundTrip(t *testing.T) {
	ctx := context.Background()
	stateDir := filepath.Join(t.TempDir(), "state")
	ledger := sampleMirrorLedger(t)
	original := append([]MirrorEntry(nil), ledger.Entries...)
	if err := SaveMirror(ctx, stateDir, ledger); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ledger.Entries, original) {
		t.Fatalf("SaveMirror mutated caller entries\ngot:  %#v\nwant: %#v", ledger.Entries, original)
	}

	path := testMirrorLedgerPath(stateDir, ledger.TargetID)
	for candidate, mode := range map[string]os.FileMode{
		filepath.Join(stateDir, "cloudflare"): 0o700,
		path:                                  0o600,
	} {
		info, err := os.Stat(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Errorf("%s permissions: %o, want %o", candidate, info.Mode().Perm(), mode)
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(raw, []byte("\n")) {
		t.Fatalf("ledger is not newline terminated: %q", raw[len(raw)-1:])
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	wantFields := []string{"version", "target_id", "endpoint", "bucket", "repository_id", "source_id", "entries"}
	if len(fields) != len(wantFields) {
		t.Fatalf("ledger JSON fields: %v, want %v", fields, wantFields)
	}
	for _, name := range wantFields {
		if _, ok := fields[name]; !ok {
			t.Errorf("ledger JSON is missing %q: %s", name, raw)
		}
	}
	var onDisk MirrorLedger
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(onDisk.Entries); i++ {
		if onDisk.Entries[i-1].Key >= onDisk.Entries[i].Key {
			t.Fatalf("entries are not sorted on disk: %q then %q", onDisk.Entries[i-1].Key, onDisk.Entries[i].Key)
		}
	}
	if !reflect.DeepEqual(onDisk, sortedMirrorLedger(ledger)) {
		t.Fatalf("on-disk ledger mismatch\ngot:  %#v\nwant: %#v", onDisk, sortedMirrorLedger(ledger))
	}

	got, err := LoadMirror(ctx, stateDir, ledger.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	if want := sortedMirrorLedger(ledger); !reflect.DeepEqual(got, want) {
		t.Fatalf("roundtrip mismatch\ngot:  %#v\nwant: %#v", got, want)
	}

	if err := SaveMirror(ctx, stateDir, ledger); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, second) {
		t.Fatal("repeated SaveMirror produced different bytes")
	}
	if matches, err := filepath.Glob(filepath.Join(stateDir, "cloudflare", ".mirror-*")); err != nil {
		t.Fatal(err)
	} else if len(matches) != 0 {
		t.Fatalf("temporary mirror files left behind: %v", matches)
	}
}

func TestMirrorLedgerEmptyEntries(t *testing.T) {
	ctx := context.Background()
	stateDir := filepath.Join(t.TempDir(), "state")
	ledger := sampleMirrorLedger(t)
	ledger.Entries = nil
	if err := SaveMirror(ctx, stateDir, ledger); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(testMirrorLedgerPath(stateDir, ledger.TargetID))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"entries":null`)) {
		t.Fatalf("empty entries encoded as null: %s", raw)
	}
	got, err := LoadMirror(ctx, stateDir, ledger.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Entries == nil || len(got.Entries) != 0 {
		t.Fatalf("loaded entries: %#v, want empty non-nil slice", got.Entries)
	}
}

func TestMirrorLedgerMissingIsZero(t *testing.T) {
	ctx := context.Background()
	stateDir := filepath.Join(t.TempDir(), "state")
	got, err := LoadMirror(ctx, stateDir, testMirrorConfig().TargetID())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, MirrorLedger{}) {
		t.Fatalf("missing ledger: %#v, want zero ledger", got)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "cloudflare")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("LoadMirror created the cloudflare directory: %v", err)
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("LoadMirror created the state directory: %v", err)
	}
}

func TestMirrorLedgerCanceledContext(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	ledger := sampleMirrorLedger(t)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := SaveMirror(canceled, stateDir, ledger); !errors.Is(err, context.Canceled) {
		t.Fatalf("SaveMirror with canceled context: %v, want context.Canceled", err)
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("canceled SaveMirror created state: %v", err)
	}

	if err := SaveMirror(context.Background(), stateDir, ledger); err != nil {
		t.Fatal(err)
	}
	path := testMirrorLedgerPath(stateDir, ledger.TargetID)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMirror(canceled, stateDir, ledger.TargetID); !errors.Is(err, context.Canceled) {
		t.Fatalf("LoadMirror with canceled context: %v, want context.Canceled", err)
	}
	if err := SaveMirror(canceled, stateDir, ledger); !errors.Is(err, context.Canceled) {
		t.Fatalf("SaveMirror over an existing ledger with canceled context: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("canceled SaveMirror replaced an existing ledger")
	}
}

func TestMirrorLoadRejectsCorruptLedger(t *testing.T) {
	ctx := context.Background()
	base := sampleMirrorLedger(t)
	cases := []struct {
		name       string
		loadTarget string
		mutate     func(*MirrorLedger)
	}{
		{"unsupported version", base.TargetID, func(l *MirrorLedger) { l.Version = 2 }},
		{"missing version", base.TargetID, func(l *MirrorLedger) { l.Version = 0 }},
		{"target id format", base.TargetID, func(l *MirrorLedger) { l.TargetID = strings.Repeat("b", 63) }},
		{"target id mismatch", base.TargetID, func(l *MirrorLedger) { l.TargetID = strings.Repeat("b", 64) }},
		{"target fields mismatch", base.TargetID, func(l *MirrorLedger) { l.Bucket = "other-images" }},
		{"content target differs from filename", base.TargetID, func(l *MirrorLedger) {
			other := testMirrorConfig()
			other.Bucket = "other-images"
			l.TargetID = other.TargetID()
			l.Bucket = other.Bucket
		}},
		{"zero repository", base.TargetID, func(l *MirrorLedger) { l.RepositoryID = domain.RepositoryID{} }},
		{"uppercase source", base.TargetID, func(l *MirrorLedger) { l.SourceID = strings.ToUpper(testMirrorSourceID) }},
		{"short source", base.TargetID, func(l *MirrorLedger) { l.SourceID = testMirrorSourceID[:63] }},
		{"duplicate key", base.TargetID, func(l *MirrorLedger) { l.Entries = append(l.Entries, l.Entries[0]) }},
		{"unknown state", base.TargetID, func(l *MirrorLedger) { l.Entries[0].State = "uploading" }},
		{"unsafe error code", base.TargetID, func(l *MirrorLedger) { l.Entries[0].ErrorCode = "Network socket reset at 10.0.0.1" }},
		{"unrecognized error code", base.TargetID, func(l *MirrorLedger) { l.Entries[1].ErrorCode = "secret_sentinel" }},
		{"error code on synced entry", base.TargetID, func(l *MirrorLedger) { l.Entries[0].ErrorCode = "network" }},
		{"synced entry without etag", base.TargetID, func(l *MirrorLedger) { l.Entries[0].ETag = "" }},
		{"invalid etag", base.TargetID, func(l *MirrorLedger) { l.Entries[0].ETag = `"quoted"` }},
		{"invalid object", base.TargetID, func(l *MirrorLedger) { l.Entries[0].Object.Size = 0 }},
		{"zero object hash", base.TargetID, func(l *MirrorLedger) { l.Entries[0].Object.Hash = domain.ObjectHash{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "state")
			ledger := cloneMirrorLedger(base)
			tc.mutate(&ledger)
			data, err := json.Marshal(ledger)
			if err != nil {
				t.Fatal(err)
			}
			path := writeMirrorRaw(t, stateDir, base.TargetID, data)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = LoadMirror(ctx, stateDir, tc.loadTarget)
			requireMirrorCorrupt(t, err)
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("LoadMirror modified a corrupt ledger")
			}
		})
	}
}

func TestMirrorLoadRejectsUnsafeEntryKeys(t *testing.T) {
	ctx := context.Background()
	base := sampleMirrorLedger(t)
	for _, key := range []string{
		"",
		".",
		"../escape.webp",
		"/absolute.webp",
		`nested\slash.webp`,
		".git/config",
		".touko/repository.json",
		"control\x00.webp",
		"trailing/",
	} {
		t.Run(key, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "state")
			ledger := cloneMirrorLedger(base)
			ledger.Entries[0].Key = key
			data, err := json.Marshal(ledger)
			if err != nil {
				t.Fatal(err)
			}
			writeMirrorRaw(t, stateDir, base.TargetID, data)
			_, err = LoadMirror(ctx, stateDir, base.TargetID)
			requireMirrorCorrupt(t, err)
		})
	}
}

func TestMirrorLoadRejectsMalformedFile(t *testing.T) {
	ctx := context.Background()
	base := sampleMirrorLedger(t)
	valid, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		data []byte
	}{
		{"empty file", nil},
		{"malformed json", []byte(`{`)},
		{"not an object", []byte(`[]`)},
		{"unknown field", bytes.Replace(valid, []byte(`"version":1`), []byte(`"version":1,"unexpected":true`), 1)},
		{"trailing data", append(append([]byte(nil), valid...), []byte("\n{}")...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "state")
			writeMirrorRaw(t, stateDir, base.TargetID, tc.data)
			_, err := LoadMirror(ctx, stateDir, base.TargetID)
			requireMirrorCorrupt(t, err)
		})
	}
}

func TestMirrorLoadRejectsOversizedFile(t *testing.T) {
	ctx := context.Background()
	base := sampleMirrorLedger(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	path := writeMirrorRaw(t, stateDir, base.TargetID, nil)
	if err := os.Truncate(path, maxMirrorLedgerBytes+1); err != nil {
		t.Fatal(err)
	}
	_, err := LoadMirror(ctx, stateDir, base.TargetID)
	requireMirrorCorrupt(t, err)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != maxMirrorLedgerBytes+1 {
		t.Fatalf("oversized ledger size changed to %d", info.Size())
	}
}

func TestMirrorLoadRejectsInvalidTargetArgument(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	for _, target := range []string{
		"",
		strings.Repeat("A", 64),
		strings.Repeat("a", 63),
		"../../escape",
		strings.Repeat("a", 64) + "/x",
	} {
		t.Run(target, func(t *testing.T) {
			if _, err := LoadMirror(ctx, stateDir, target); err == nil {
				t.Fatalf("LoadMirror accepted target %q", target)
			}
		})
	}
}

func TestMirrorSaveRejectsInvalidLedgerAtomically(t *testing.T) {
	ctx := context.Background()
	stateDir := filepath.Join(t.TempDir(), "state")
	base := sampleMirrorLedger(t)
	if err := SaveMirror(ctx, stateDir, base); err != nil {
		t.Fatal(err)
	}
	path := testMirrorLedgerPath(stateDir, base.TargetID)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*MirrorLedger)
	}{
		{"unsupported version", func(l *MirrorLedger) { l.Version = 2 }},
		{"target id mismatch", func(l *MirrorLedger) { l.TargetID = strings.Repeat("0", 64) }},
		{"invalid endpoint", func(l *MirrorLedger) { l.Endpoint = "http://account.r2.cloudflarestorage.com" }},
		{"invalid bucket", func(l *MirrorLedger) { l.Bucket = "bad/bucket" }},
		{"zero repository", func(l *MirrorLedger) { l.RepositoryID = domain.RepositoryID{} }},
		{"invalid source", func(l *MirrorLedger) { l.SourceID = strings.ToUpper(testMirrorSourceID) }},
		{"unsafe key", func(l *MirrorLedger) { l.Entries[0].Key = "../escape.webp" }},
		{"duplicate key", func(l *MirrorLedger) { l.Entries = append(l.Entries, l.Entries[0]) }},
		{"unknown state", func(l *MirrorLedger) { l.Entries[0].State = "uploading" }},
		{"unsafe error code", func(l *MirrorLedger) { l.Entries[1].ErrorCode = "boom!" }},
		{"unrecognized error code", func(l *MirrorLedger) { l.Entries[1].ErrorCode = "secret_sentinel" }},
		{"error code on synced entry", func(l *MirrorLedger) { l.Entries[0].ErrorCode = "network" }},
		{"synced entry without etag", func(l *MirrorLedger) { l.Entries[0].ETag = "" }},
		{"invalid object", func(l *MirrorLedger) { l.Entries[0].Object.Ext = "tiff" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ledger := cloneMirrorLedger(base)
			tc.mutate(&ledger)
			err := SaveMirror(ctx, stateDir, ledger)
			requireMirrorCorrupt(t, err)
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("invalid SaveMirror replaced the existing ledger")
			}
			matches, err := filepath.Glob(filepath.Join(stateDir, "cloudflare", ".mirror-*"))
			if err != nil {
				t.Fatal(err)
			}
			if len(matches) != 0 {
				t.Fatalf("invalid SaveMirror left temporary files: %v", matches)
			}
		})
	}
}

func TestMirrorTargetIdentityMatchesR2Config(t *testing.T) {
	ctx := context.Background()
	upper := testMirrorConfig()
	lower := upper
	lower.Endpoint = "https://account.r2.cloudflarestorage.com"
	if upper.TargetID() != lower.TargetID() {
		t.Fatalf("r2.Config target normalization changed: %q != %q", upper.TargetID(), lower.TargetID())
	}
	sameBucketOtherEndpoint := lower
	sameBucketOtherEndpoint.Endpoint = "https://account.eu.r2.cloudflarestorage.com"
	if sameBucketOtherEndpoint.TargetID() == lower.TargetID() {
		t.Fatal("r2.Config target id ignores the endpoint")
	}
	otherBucket := lower
	otherBucket.Bucket = "other-images"
	if otherBucket.TargetID() == lower.TargetID() {
		t.Fatal("r2.Config target id ignores the bucket")
	}

	for name, cfg := range map[string]r2.Config{
		"raw endpoint":   upper,
		"normalized":     lower,
		"other endpoint": sameBucketOtherEndpoint,
		"other bucket":   otherBucket,
	} {
		t.Run(name, func(t *testing.T) {
			targetID := cfg.TargetID()
			if len(targetID) != 64 || targetID != strings.ToLower(targetID) {
				t.Fatalf("r2.Config.TargetID() = %q, want 64 lowercase hex characters", targetID)
			}

			stateDir := filepath.Join(t.TempDir(), "state")
			ledger := sampleMirrorLedger(t)
			ledger.TargetID = targetID
			ledger.Endpoint = cfg.Endpoint
			ledger.Bucket = cfg.Bucket
			ledger.Entries = []MirrorEntry{}
			if err := SaveMirror(ctx, stateDir, ledger); err != nil {
				t.Fatal(err)
			}
			got, err := LoadMirror(ctx, stateDir, targetID)
			if err != nil {
				t.Fatal(err)
			}
			if got.TargetID != targetID || got.Endpoint != cfg.Endpoint || got.Bucket != cfg.Bucket ||
				got.Version != ledger.Version || got.RepositoryID != ledger.RepositoryID || got.SourceID != ledger.SourceID ||
				len(got.Entries) != 0 {
				t.Fatalf("target roundtrip mismatch: %#v, want %#v", got, ledger)
			}
		})
	}
}

func TestMirrorSaveRejectsInvalidTarget(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		cfg  r2.Config
	}{
		{"plain http", r2.Config{Endpoint: "http://account.r2.cloudflarestorage.com", Bucket: "touko-images"}},
		{"userinfo", r2.Config{Endpoint: "https://user:pass@account.r2.cloudflarestorage.com", Bucket: "touko-images"}},
		{"path", r2.Config{Endpoint: "https://account.r2.cloudflarestorage.com/path", Bucket: "touko-images"}},
		{"query", r2.Config{Endpoint: "https://account.r2.cloudflarestorage.com?x=1", Bucket: "touko-images"}},
		{"fragment", r2.Config{Endpoint: "https://account.r2.cloudflarestorage.com#x", Bucket: "touko-images"}},
		{"empty endpoint", r2.Config{Bucket: "touko-images"}},
		{"bucket separator", r2.Config{Endpoint: "https://account.r2.cloudflarestorage.com", Bucket: "bad/bucket"}},
		{"bucket whitespace", r2.Config{Endpoint: "https://account.r2.cloudflarestorage.com", Bucket: "bad bucket"}},
		{"empty bucket", r2.Config{Endpoint: "https://account.r2.cloudflarestorage.com"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "state")
			ledger := sampleMirrorLedger(t)
			ledger.TargetID = tc.cfg.TargetID()
			ledger.Endpoint = tc.cfg.Endpoint
			ledger.Bucket = tc.cfg.Bucket
			requireMirrorCorrupt(t, SaveMirror(ctx, stateDir, ledger))
			if _, err := os.Stat(filepath.Join(stateDir, "cloudflare")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("invalid target created the cloudflare directory: %v", err)
			}
		})
	}
}

func TestMirrorLedgerSurvivesReindex(t *testing.T) {
	ctx := context.Background()
	stateDir := filepath.Join(t.TempDir(), "state")
	ledger := sampleMirrorLedger(t)
	if err := SaveMirror(ctx, stateDir, ledger); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(stateDir, "cloudflare", "unrelated.txt")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Rebuild(ctx, stateDir, sampleState(t)); err != nil {
		t.Fatal(err)
	}
	got, err := LoadMirror(ctx, stateDir, ledger.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	if want := sortedMirrorLedger(ledger); !reflect.DeepEqual(got, want) {
		t.Fatalf("reindex changed the mirror ledger\ngot:  %#v\nwant: %#v", got, want)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("reindex removed unrelated mirror files: %v", err)
	}
}
