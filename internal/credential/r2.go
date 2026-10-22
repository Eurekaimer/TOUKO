package credential

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode"

	"github.com/Eurekaimer/TOUKO/internal/atomicfile"
)

// r2CredentialsVersion is the durable format of the private workspace file.
const r2CredentialsVersion = 1

// r2CredentialsLimit bounds the credential file far below any legitimate size.
const r2CredentialsLimit = 64 << 10

// R2Pair is one Cloudflare R2 key pair. It is deliberately never serialized
// into API responses, status output or error messages.
type R2Pair struct {
	AccessKeyID     string
	SecretAccessKey string
}

// r2CredentialsPath is the private, workspace-scoped credential file. It lives
// beside the optional mirror ledgers, so each workspace keeps its own secret
// even when the same bucket is shared between them.
func r2CredentialsPath(dir string) string {
	return filepath.Join(dir, "cloudflare", "credentials.json")
}

type r2CredentialsFile struct {
	Version         int    `json:"version"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
}

// ResolveR2 resolves the workspace's Cloudflare R2 credentials. The dedicated
// process environment variables win, but only as a complete pair; when neither
// variable is set the private workspace file saved by the settings UI is used.
// Mixing one environment variable with one stored value would make the
// effective pair unpredictable, so a partially set environment is rejected
// instead of silently falling back. Errors never contain credential values.
func ResolveR2(ctx context.Context, dir string) (R2Pair, error) {
	if err := ctx.Err(); err != nil {
		return R2Pair{}, err
	}
	pair, err := r2FromEnvironment()
	if err != nil {
		return R2Pair{}, err
	}
	if pair.AccessKeyID != "" {
		return pair, nil
	}
	stored, found, err := LoadR2(ctx, dir)
	if err != nil {
		return R2Pair{}, err
	}
	if !found {
		return R2Pair{}, errors.New("Cloudflare R2 credentials are not configured; set TOUKO_R2_ACCESS_KEY_ID and TOUKO_R2_SECRET_ACCESS_KEY, or save them in the workspace settings")
	}
	return stored, nil
}

// EnvironmentR2 reports the process R2 pair without reading the workspace
// file. configured is false when neither variable is set; a partial or invalid
// pair is an error that names only the offending variable.
func EnvironmentR2() (R2Pair, bool, error) {
	pair, err := r2FromEnvironment()
	if err != nil {
		return R2Pair{}, false, err
	}
	if pair.AccessKeyID == "" {
		return R2Pair{}, false, nil
	}
	return pair, true, nil
}

func r2FromEnvironment() (R2Pair, error) {
	accessKeyID := os.Getenv("TOUKO_R2_ACCESS_KEY_ID")
	secretAccessKey := os.Getenv("TOUKO_R2_SECRET_ACCESS_KEY")
	if accessKeyID == "" && secretAccessKey == "" {
		return R2Pair{}, nil
	}
	if accessKeyID == "" || secretAccessKey == "" {
		return R2Pair{}, errors.New("TOUKO_R2_ACCESS_KEY_ID and TOUKO_R2_SECRET_ACCESS_KEY must be set together")
	}
	if !validR2Value(accessKeyID) {
		return R2Pair{}, errors.New("invalid TOUKO_R2_ACCESS_KEY_ID credential")
	}
	if !validR2Value(secretAccessKey) {
		return R2Pair{}, errors.New("invalid TOUKO_R2_SECRET_ACCESS_KEY credential")
	}
	return R2Pair{AccessKeyID: accessKeyID, SecretAccessKey: secretAccessKey}, nil
}

// LoadR2 reads the private workspace credential file. A missing file is
// reported as found=false; every other failure is a controlled error that
// never repeats the file contents.
func LoadR2(ctx context.Context, dir string) (R2Pair, bool, error) {
	if err := ctx.Err(); err != nil {
		return R2Pair{}, false, err
	}
	path := r2CredentialsPath(dir)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return R2Pair{}, false, nil
	}
	if err != nil {
		return R2Pair{}, false, errors.New("workspace Cloudflare R2 credentials are unreadable")
	}
	if !info.Mode().IsRegular() || info.Size() > r2CredentialsLimit {
		return R2Pair{}, false, errors.New("workspace Cloudflare R2 credential file is invalid")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return R2Pair{}, false, errors.New("workspace Cloudflare R2 credentials are unreadable")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var stored r2CredentialsFile
	if err := decoder.Decode(&stored); err != nil {
		return R2Pair{}, false, errors.New("workspace Cloudflare R2 credential file is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return R2Pair{}, false, errors.New("workspace Cloudflare R2 credential file is invalid")
	}
	if stored.Version != r2CredentialsVersion || !validR2Value(stored.AccessKeyID) || !validR2Value(stored.SecretAccessKey) {
		return R2Pair{}, false, errors.New("workspace Cloudflare R2 credential file is invalid")
	}
	return R2Pair{AccessKeyID: stored.AccessKeyID, SecretAccessKey: stored.SecretAccessKey}, true, nil
}

// SaveR2 atomically replaces the private workspace credential file. The file is
// 0600 inside a 0700 directory and is never written into config.json, the
// mirror ledgers, SQLite or the publication journal.
func SaveR2(ctx context.Context, dir string, pair R2Pair) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateR2Pair(pair); err != nil {
		return err
	}
	path := r2CredentialsPath(dir)
	base := filepath.Dir(path)
	if err := os.MkdirAll(base, 0700); err != nil {
		return fmt.Errorf("cannot create the Cloudflare credential directory: %w", err)
	}
	if err := os.Chmod(base, 0700); err != nil {
		return fmt.Errorf("cannot secure the Cloudflare credential directory: %w", err)
	}
	data, err := json.Marshal(r2CredentialsFile{
		Version:         r2CredentialsVersion,
		AccessKeyID:     pair.AccessKeyID,
		SecretAccessKey: pair.SecretAccessKey,
	})
	if err != nil {
		return err
	}
	if err := atomicfile.Write(ctx, path, append(data, '\n'), 0600); err != nil {
		return fmt.Errorf("cannot save the workspace Cloudflare R2 credentials: %w", err)
	}
	return nil
}

// ValidateR2Pair validates a pair without reading the environment or the
// workspace file. Messages name the offending field and never its value.
func ValidateR2Pair(pair R2Pair) error {
	if !validR2Value(pair.AccessKeyID) {
		return errors.New("the Cloudflare R2 access key id must be non-empty without whitespace or control characters")
	}
	if !validR2Value(pair.SecretAccessKey) {
		return errors.New("the Cloudflare R2 secret access key must be non-empty without whitespace or control characters")
	}
	return nil
}

func validR2Value(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}
