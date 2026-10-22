package credential_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/credential"
)

func setR2Environment(t *testing.T, accessKeyID, secretAccessKey string) {
	t.Helper()
	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", accessKeyID)
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", secretAccessKey)
}

func saveR2Pair(t *testing.T, dir, accessKeyID, secretAccessKey string) {
	t.Helper()
	if err := credential.SaveR2(context.Background(), dir, credential.R2Pair{AccessKeyID: accessKeyID, SecretAccessKey: secretAccessKey}); err != nil {
		t.Fatal(err)
	}
}

func TestResolveR2PrefersEnvironmentThenWorkspaceFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	setR2Environment(t, "", "")

	if _, err := credential.ResolveR2(ctx, dir); err == nil {
		t.Fatal("resolved R2 credentials without an environment or workspace source")
	}

	saveR2Pair(t, dir, "stored-access-SENTINEL", "stored-secret-SENTINEL")
	pair, err := credential.ResolveR2(ctx, dir)
	if err != nil || pair.AccessKeyID != "stored-access-SENTINEL" || pair.SecretAccessKey != "stored-secret-SENTINEL" {
		t.Fatalf("stored workspace credentials not used: %+v %v", pair, err)
	}

	setR2Environment(t, "env-access-SENTINEL", "env-secret-SENTINEL")
	pair, err = credential.ResolveR2(ctx, dir)
	if err != nil || pair.AccessKeyID != "env-access-SENTINEL" || pair.SecretAccessKey != "env-secret-SENTINEL" {
		t.Fatalf("environment did not override the workspace file: %+v %v", pair, err)
	}
}

func TestResolveR2RejectsPartialEnvironmentInsteadOfFallingBack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	saveR2Pair(t, dir, "stored-access-SENTINEL", "stored-secret-SENTINEL")
	setR2Environment(t, "env-access-SENTINEL", "")
	_, err := credential.ResolveR2(ctx, dir)
	if err == nil {
		t.Fatal("partially configured environment silently fell back to the workspace file")
	}
	if !strings.Contains(err.Error(), "TOUKO_R2_ACCESS_KEY_ID") || !strings.Contains(err.Error(), "TOUKO_R2_SECRET_ACCESS_KEY") {
		t.Fatalf("partial environment error does not name both variables: %v", err)
	}
	if strings.Contains(err.Error(), "SENTINEL") {
		t.Fatal("partial environment error disclosed a credential value")
	}
}

func TestResolveR2RejectsInvalidValuesWithoutDisclosure(t *testing.T) {
	for _, tc := range []struct {
		name, access, secret, variable string
	}{
		{"blank access", " ", "secret-SENTINEL", "TOUKO_R2_ACCESS_KEY_ID"},
		{"tab access", "access\tSENTINEL", "secret-SENTINEL", "TOUKO_R2_ACCESS_KEY_ID"},
		{"newline access", "access\nSENTINEL", "secret-SENTINEL", "TOUKO_R2_ACCESS_KEY_ID"},
		{"blank secret", "access-SENTINEL", " ", "TOUKO_R2_SECRET_ACCESS_KEY"},
		{"space secret", "access-SENTINEL", "secret SENTINEL", "TOUKO_R2_SECRET_ACCESS_KEY"},
		{"carriage return secret", "access-SENTINEL", "secret\rSENTINEL", "TOUKO_R2_SECRET_ACCESS_KEY"},
		{"non-breaking space secret", "access-SENTINEL", "secret\u00a0SENTINEL", "TOUKO_R2_SECRET_ACCESS_KEY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setR2Environment(t, tc.access, tc.secret)
			pair, err := credential.ResolveR2(context.Background(), t.TempDir())
			if err == nil || pair != (credential.R2Pair{}) {
				t.Fatalf("invalid R2 credential accepted: %+v %v", pair, err)
			}
			if !strings.Contains(err.Error(), tc.variable) {
				t.Fatalf("error does not name %s: %v", tc.variable, err)
			}
			if strings.Contains(err.Error(), "SENTINEL") {
				t.Fatal("invalid credential error disclosed a credential value")
			}
		})
	}
}

func TestResolveR2DoesNotFallBackToOtherCredentials(t *testing.T) {
	setR2Environment(t, "", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "aws-access-SENTINEL")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret-SENTINEL")
	t.Setenv("GITHUB_TOKEN", "github-SENTINEL")
	t.Setenv("GH_TOKEN", "gh-SENTINEL")
	t.Setenv("PATH", t.TempDir())
	pair, err := credential.ResolveR2(context.Background(), t.TempDir())
	if err == nil || pair != (credential.R2Pair{}) {
		t.Fatalf("R2 credentials fell back to another source: %+v %v", pair, err)
	}
	if strings.Contains(err.Error(), "SENTINEL") {
		t.Fatal("fallback error disclosed a credential value")
	}
}

func TestResolveR2RespectsCancellation(t *testing.T) {
	setR2Environment(t, "access-SENTINEL", "secret-SENTINEL")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pair, err := credential.ResolveR2(ctx, t.TempDir())
	if !errors.Is(err, context.Canceled) || pair != (credential.R2Pair{}) {
		t.Fatalf("cancellation not preserved: %+v %v", pair, err)
	}
}

func TestSaveR2RoundTripsInAPrivateWorkspaceFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	saveR2Pair(t, dir, "access-SENTINEL", "secret-SENTINEL")

	path := filepath.Join(dir, "cloudflare", "credentials.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("credential file permissions: %o, want 0600", info.Mode().Perm())
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if parent.Mode().Perm() != 0700 {
		t.Fatalf("credential directory permissions: %o, want 0700", parent.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("saving credentials created config.json: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "cloudflare"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("cloudflare directory contents: %v %v", entries, err)
	}

	stored, found, err := credential.LoadR2(ctx, dir)
	if err != nil || !found || stored.AccessKeyID != "access-SENTINEL" || stored.SecretAccessKey != "secret-SENTINEL" {
		t.Fatalf("LoadR2 = %+v, %v, %v", stored, found, err)
	}
}

func TestSaveR2RejectsInvalidPairsWithoutDisclosure(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		pair  credential.R2Pair
		field string
	}{
		{"empty", credential.R2Pair{}, "access key id"},
		{"blank access", credential.R2Pair{AccessKeyID: " ", SecretAccessKey: "secret-SENTINEL"}, "access key id"},
		{"space secret", credential.R2Pair{AccessKeyID: "access-SENTINEL", SecretAccessKey: "secret SENTINEL"}, "secret access key"},
		{"control secret", credential.R2Pair{AccessKeyID: "access-SENTINEL", SecretAccessKey: "secret\nSENTINEL"}, "secret access key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			err := credential.SaveR2(ctx, dir, tc.pair)
			if err == nil {
				t.Fatal("invalid credential pair was saved")
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("error does not name %q: %v", tc.field, err)
			}
			if strings.Contains(err.Error(), "SENTINEL") {
				t.Fatal("validation error disclosed a credential value")
			}
			if _, statErr := os.Stat(filepath.Join(dir, "cloudflare", "credentials.json")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("invalid pair left a credential file: %v", statErr)
			}
		})
	}
}

func TestLoadR2RejectsMalformedStoredPairWithoutDisclosure(t *testing.T) {
	ctx := context.Background()
	for name, content := range map[string]string{
		"unknown field": `{"version":1,"access_key_id":"access-SENTINEL","secret_access_key":"secret-SENTINEL","extra":"x"}`,
		"wrong version": `{"version":2,"access_key_id":"access-SENTINEL","secret_access_key":"secret-SENTINEL"}`,
		"blank secret":  `{"version":1,"access_key_id":"access-SENTINEL","secret_access_key":" "}`,
		"trailing":      `{"version":1,"access_key_id":"access-SENTINEL","secret_access_key":"secret-SENTINEL"}{}`,
		"not json":      `secret-SENTINEL`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			base := filepath.Join(dir, "cloudflare")
			if err := os.MkdirAll(base, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(base, "credentials.json"), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			pair, found, err := credential.LoadR2(ctx, dir)
			if err == nil || found || pair != (credential.R2Pair{}) {
				t.Fatalf("malformed credential file accepted: %+v %v %v", pair, found, err)
			}
			if strings.Contains(err.Error(), "SENTINEL") {
				t.Fatal("malformed file error disclosed a credential value")
			}
		})
	}
}

func TestResolveR2WorkspaceIsolation(t *testing.T) {
	ctx := context.Background()
	setR2Environment(t, "", "")
	first := t.TempDir()
	second := t.TempDir()
	saveR2Pair(t, first, "first-access-SENTINEL", "first-secret-SENTINEL")
	saveR2Pair(t, second, "second-access-SENTINEL", "second-secret-SENTINEL")

	firstPair, err := credential.ResolveR2(ctx, first)
	if err != nil || firstPair.AccessKeyID != "first-access-SENTINEL" {
		t.Fatalf("first workspace pair: %+v %v", firstPair, err)
	}
	secondPair, err := credential.ResolveR2(ctx, second)
	if err != nil || secondPair.AccessKeyID != "second-access-SENTINEL" {
		t.Fatalf("second workspace pair: %+v %v", secondPair, err)
	}
	if firstPair == secondPair {
		t.Fatal("workspaces shared one credential pair")
	}
}
