package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/r2"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

func cloudflareConfigJSON(extra string) string {
	fields := `"backend":"github","branch":"main","owner":"Eurekaimer","name":"TOUKO-WEBP-01","private":false`
	if extra == "" {
		return "{" + fields + "}\n"
	}
	return "{" + fields + "," + extra + "}\n"
}

func writeCloudflareConfigTestFile(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func readCloudflareConfigTestFile(t *testing.T, dir string) ([]byte, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return b, true
}

func assertCloudflareConfigInvalid(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a configuration error")
	}
	if code := r2.ErrorCode(err); code != "configuration_invalid" {
		t.Fatalf("error code = %q; want configuration_invalid (%v)", code, err)
	}
	var target *r2.Error
	if !errors.As(err, &target) || target.Op != "config" {
		t.Fatalf("error is not an r2 config error: %v", err)
	}
}

func TestCloudflareConfigDisabled(t *testing.T) {
	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", "invalid value")
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "\tsecret")
	for _, tc := range []struct {
		name, content string
		write         bool
	}{
		{"missing file", "", false},
		{"legacy config only", cloudflareConfigJSON(""), true},
		{"null block", cloudflareConfigJSON(`"cloudflare":null`), true},
		{"disabled with invalid values", cloudflareConfigJSON(`"cloudflare":{"enabled":false,"endpoint":"not a url","bucket":"bad/bucket","public_base_url":"http://img.example.com"}`), true},
		{"missing enabled with invalid values", cloudflareConfigJSON(`"cloudflare":{"endpoint":"not a url"}`), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.write {
				writeCloudflareConfigTestFile(t, dir, tc.content)
			}
			before, hadFile := readCloudflareConfigTestFile(t, dir)
			cfg, err := (&Service{Dir: dir}).CloudflareConfig(context.Background())
			if err != nil {
				t.Fatalf("disabled Cloudflare configuration failed: %v", err)
			}
			if cfg != (r2.Config{}) {
				t.Fatalf("disabled Cloudflare configuration = %+v; want zero", cfg)
			}
			after, haveFile := readCloudflareConfigTestFile(t, dir)
			if hadFile != haveFile || !bytes.Equal(before, after) {
				t.Fatal("CloudflareConfig rewrote the configuration file")
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Name() == "cloudflare" {
					t.Fatal("disabled Cloudflare configuration created a mirror ledger")
				}
			}
		})
	}
}

func TestCloudflareConfigEnabled(t *testing.T) {
	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", " ")
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "invalid secret")
	dir := t.TempDir()
	content := cloudflareConfigJSON(`"cloudflare":{"enabled":true,"endpoint":"https://acct.r2.cloudflarestorage.com","bucket":"touko-images","public_base_url":"https://img.eurekaimer.icu"}`)
	writeCloudflareConfigTestFile(t, dir, content)
	before, _ := readCloudflareConfigTestFile(t, dir)
	cfg, err := (&Service{Dir: dir}).CloudflareConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := r2.Config{
		Enabled:       true,
		Endpoint:      "https://acct.r2.cloudflarestorage.com",
		Bucket:        "touko-images",
		PublicBaseURL: "https://img.eurekaimer.icu",
	}
	if cfg != want {
		t.Fatalf("CloudflareConfig = %+v; want %+v", cfg, want)
	}
	after, _ := readCloudflareConfigTestFile(t, dir)
	if !bytes.Equal(before, after) {
		t.Fatal("CloudflareConfig rewrote the configuration file")
	}
}

func TestCloudflareConfigIgnoresUnrelatedOuterFields(t *testing.T) {
	dir := t.TempDir()
	content := cloudflareConfigJSON(`"appearance":{"scale":1.25},"cloudflare":{"enabled":true,"endpoint":"https://acct.eu.r2.cloudflarestorage.com/","bucket":"touko-images","public_base_url":"https://img.eurekaimer.icu/base/"}`)
	writeCloudflareConfigTestFile(t, dir, content)
	cfg, err := (&Service{Dir: dir}).CloudflareConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled || cfg.Endpoint != "https://acct.eu.r2.cloudflarestorage.com/" || cfg.PublicBaseURL != "https://img.eurekaimer.icu/base/" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestCloudflareConfigRejectsInvalidEnabledConfiguration(t *testing.T) {
	base := func(block string) string { return cloudflareConfigJSON(`"cloudflare":` + block) }
	for _, tc := range []struct {
		name, content string
	}{
		{"missing endpoint", base(`{"enabled":true,"bucket":"touko-images","public_base_url":"https://img.example.com"}`)},
		{"http endpoint", base(`{"enabled":true,"endpoint":"http://acct.r2.cloudflarestorage.com","bucket":"touko-images","public_base_url":"https://img.example.com"}`)},
		{"endpoint credentials", base(`{"enabled":true,"endpoint":"https://user:hunter2@acct.r2.cloudflarestorage.com","bucket":"touko-images","public_base_url":"https://img.example.com"}`)},
		{"endpoint path", base(`{"enabled":true,"endpoint":"https://acct.r2.cloudflarestorage.com/bucket","bucket":"touko-images","public_base_url":"https://img.example.com"}`)},
		{"endpoint query", base(`{"enabled":true,"endpoint":"https://acct.r2.cloudflarestorage.com?x=1","bucket":"touko-images","public_base_url":"https://img.example.com"}`)},
		{"missing bucket", base(`{"enabled":true,"endpoint":"https://acct.r2.cloudflarestorage.com","public_base_url":"https://img.example.com"}`)},
		{"bucket separator", base(`{"enabled":true,"endpoint":"https://acct.r2.cloudflarestorage.com","bucket":"touko/images","public_base_url":"https://img.example.com"}`)},
		{"missing public base", base(`{"enabled":true,"endpoint":"https://acct.r2.cloudflarestorage.com","bucket":"touko-images"}`)},
		{"http public base", base(`{"enabled":true,"endpoint":"https://acct.r2.cloudflarestorage.com","bucket":"touko-images","public_base_url":"http://img.example.com"}`)},
		{"public base traversal", base(`{"enabled":true,"endpoint":"https://acct.r2.cloudflarestorage.com","bucket":"touko-images","public_base_url":"https://img.example.com/%2e%2e/secret"}`)},
		{"unknown block field", base(`{"enabled":true,"endpoint":"https://acct.r2.cloudflarestorage.com","bucket":"touko-images","public_base_url":"https://img.example.com","secret":"value"}`)},
		{"wrong enabled type", base(`{"enabled":"yes","endpoint":"https://acct.r2.cloudflarestorage.com","bucket":"touko-images","public_base_url":"https://img.example.com"}`)},
		{"wrong bucket type", base(`{"enabled":true,"endpoint":"https://acct.r2.cloudflarestorage.com","bucket":7,"public_base_url":"https://img.example.com"}`)},
		{"non-object block", base(`[]`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeCloudflareConfigTestFile(t, dir, tc.content)
			before, _ := readCloudflareConfigTestFile(t, dir)
			cfg, err := (&Service{Dir: dir}).CloudflareConfig(context.Background())
			assertCloudflareConfigInvalid(t, err)
			if cfg != (r2.Config{}) {
				t.Fatalf("invalid configuration returned %+v", cfg)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Fatal("configuration error disclosed endpoint credentials")
			}
			after, _ := readCloudflareConfigTestFile(t, dir)
			if !bytes.Equal(before, after) {
				t.Fatal("CloudflareConfig rewrote the invalid configuration file")
			}
		})
	}
}

func TestCloudflareConfigMalformedFile(t *testing.T) {
	for _, content := range []string{"{", "[]", `{"cloudflare":`} {
		dir := t.TempDir()
		writeCloudflareConfigTestFile(t, dir, content)
		cfg, err := (&Service{Dir: dir}).CloudflareConfig(context.Background())
		assertCloudflareConfigInvalid(t, err)
		if cfg != (r2.Config{}) {
			t.Fatalf("malformed configuration returned %+v", cfg)
		}
	}
}

func TestCloudflareConfigPreservesLegacyLocation(t *testing.T) {
	dir := t.TempDir()
	content := cloudflareConfigJSON(`"cloudflare":{"enabled":true,"endpoint":"https://acct.r2.cloudflarestorage.com","bucket":"touko-images","public_base_url":"https://img.eurekaimer.icu"}`)
	writeCloudflareConfigTestFile(t, dir, content)
	s := &Service{Dir: dir}
	want := repository.Location{Backend: "github", Branch: "main", Owner: "Eurekaimer", Name: "TOUKO-WEBP-01"}
	got, err := s.Location(context.Background())
	if err != nil || got != want {
		t.Fatalf("Location() = %+v, %v; want %+v", got, err, want)
	}
	key := map[repository.Location]bool{got: true}
	if !key[want] {
		t.Fatal("Location equality changed under an optional cloudflare block")
	}
	cfg, err := s.CloudflareConfig(context.Background())
	if err != nil || !cfg.Enabled {
		t.Fatalf("CloudflareConfig() = %+v, %v", cfg, err)
	}
	after, ok := readCloudflareConfigTestFile(t, dir)
	if !ok || !bytes.Equal(after, []byte(content)) {
		t.Fatal("configuration file changed during reads")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "config.json" {
		t.Fatalf("unexpected state directory contents: %v %v", entries, err)
	}
}

func TestCloudflareConfigRespectsCancellation(t *testing.T) {
	dir := t.TempDir()
	writeCloudflareConfigTestFile(t, dir, cloudflareConfigJSON(`"cloudflare":{"enabled":true,"endpoint":"https://acct.r2.cloudflarestorage.com","bucket":"touko-images","public_base_url":"https://img.example.com"}`))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&Service{Dir: dir}).CloudflareConfig(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not preserved: %v", err)
	}
}
