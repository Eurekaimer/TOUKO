package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/credential"
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

func readConfigFieldsTest(t *testing.T, dir string) map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	fields := make(map[string]json.RawMessage)
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func TestCloudflareSettingsPreserveLegacyConfiguration(t *testing.T) {
	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", "")
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "")
	dir := t.TempDir()
	content := cloudflareConfigJSON(`"appearance":{"scale":1.25},"notes":"keep me","cloudflare":{"enabled":false,"endpoint":"https://old.r2.cloudflarestorage.com","bucket":"old-bucket","public_base_url":"https://old.example.test/base"}`)
	writeCloudflareConfigTestFile(t, dir, content)
	service := &Service{Dir: dir}
	ctx := context.Background()

	// A disabled block keeps its target fields so the UI can toggle off and on
	// again without losing them, and reading requires no credentials.
	view, err := service.CloudflareSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if view.Enabled || view.Endpoint != "https://old.r2.cloudflarestorage.com" || view.Bucket != "old-bucket" || view.PublicBaseURL != "https://old.example.test/base" {
		t.Fatalf("disabled settings were not retained: %+v", view)
	}
	if view.CredentialsConfigured {
		t.Fatal("credentials reported configured without a source")
	}

	updated, err := service.UpdateCloudflareSettings(ctx, CloudflareConfigInput{
		Enabled:         true,
		Endpoint:        "https://acct.r2.cloudflarestorage.com",
		Bucket:          "touko-images",
		PublicBaseURL:   "https://img.eurekaimer.icu",
		AccessKeyID:     "access-SENTINEL",
		SecretAccessKey: "secret-SENTINEL",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Enabled || updated.Endpoint != "https://acct.r2.cloudflarestorage.com" || !updated.CredentialsConfigured {
		t.Fatalf("updated settings = %+v", updated)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dir, "config.json")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("config.json permissions: %v %v", info, err)
	}
	if bytes.Contains(raw, []byte("SENTINEL")) || bytes.Contains(raw, []byte("secret")) {
		t.Fatal("config.json contains credential material")
	}
	fields := readConfigFieldsTest(t, dir)
	for field, want := range map[string]string{
		"backend":    `"github"`,
		"branch":     `"main"`,
		"owner":      `"Eurekaimer"`,
		"name":       `"TOUKO-WEBP-01"`,
		"private":    `false`,
		"appearance": `{"scale":1.25}`,
		"notes":      `"keep me"`,
	} {
		got, ok := fields[field]
		var compact bytes.Buffer
		if !ok || json.Compact(&compact, got) != nil || compact.String() != want {
			t.Fatalf("config field %q = %s, want %s", field, got, want)
		}
	}
	stored, err := credential.ResolveR2(ctx, dir)
	if err != nil || stored.AccessKeyID != "access-SENTINEL" || stored.SecretAccessKey != "secret-SENTINEL" {
		t.Fatalf("stored credentials = %+v, %v", stored, err)
	}
	credentialPath := filepath.Join(dir, "cloudflare", "credentials.json")
	if info, err := os.Stat(credentialPath); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("credential file permissions: %v %v", info, err)
	}
	if info, err := os.Stat(filepath.Dir(credentialPath)); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("credential directory permissions: %v %v", info, err)
	}

	// The legacy location remains exactly comparable and decodable.
	location, err := service.Location(ctx)
	want := repository.Location{Backend: "github", Branch: "main", Owner: "Eurekaimer", Name: "TOUKO-WEBP-01"}
	if err != nil || location != want {
		t.Fatalf("Location() = %+v, %v; want %+v", location, err, want)
	}
	cfg, err := (&Service{Dir: dir}).CloudflareConfig(ctx)
	if err != nil || !cfg.Enabled || cfg != (r2.Config{
		Enabled:       true,
		Endpoint:      "https://acct.r2.cloudflarestorage.com",
		Bucket:        "touko-images",
		PublicBaseURL: "https://img.eurekaimer.icu",
	}) {
		t.Fatalf("reopened CloudflareConfig = %+v, %v", cfg, err)
	}
}

func TestCloudflareSettingsKeepSavedCredentialsAndRejectPartialPairs(t *testing.T) {
	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", "")
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "")
	dir := t.TempDir()
	writeCloudflareConfigTestFile(t, dir, cloudflareConfigJSON(""))
	service := &Service{Dir: dir}
	ctx := context.Background()

	// Disabling with both credential fields empty must not require any source.
	view, err := service.UpdateCloudflareSettings(ctx, CloudflareConfigInput{Enabled: false})
	if err != nil {
		t.Fatalf("disabling without credentials failed: %v", err)
	}
	if view.CredentialsConfigured {
		t.Fatal("credentials reported configured without a source")
	}

	save := func() []byte {
		t.Helper()
		_, err := service.UpdateCloudflareSettings(ctx, CloudflareConfigInput{
			AccessKeyID: "access-SENTINEL", SecretAccessKey: "secret-SENTINEL",
		})
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(dir, "cloudflare", "credentials.json"))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	before := save()
	view, err = service.UpdateCloudflareSettings(ctx, CloudflareConfigInput{Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	if !view.CredentialsConfigured {
		t.Fatal("blank credential fields discarded the saved pair")
	}
	after, err := os.ReadFile(filepath.Join(dir, "cloudflare", "credentials.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("blank credential fields rewrote the stored pair: %v", err)
	}
	configBefore, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	for _, input := range []CloudflareConfigInput{
		{AccessKeyID: "only-access-SENTINEL"},
		{SecretAccessKey: "only-secret-SENTINEL"},
	} {
		_, err := service.UpdateCloudflareSettings(ctx, input)
		var settings *CloudflareConfigError
		if !errors.As(err, &settings) || settings.Code != "configuration_invalid" {
			t.Fatalf("partial credential pair error = %v", err)
		}
		if strings.Contains(err.Error(), "SENTINEL") {
			t.Fatal("partial credential error disclosed a value")
		}
		configAfter, readErr := os.ReadFile(filepath.Join(dir, "config.json"))
		if readErr != nil || !bytes.Equal(configBefore, configAfter) {
			t.Fatal("rejected credential pair rewrote config.json")
		}
		storedAfter, statErr := os.ReadFile(filepath.Join(dir, "cloudflare", "credentials.json"))
		if statErr != nil || !bytes.Equal(before, storedAfter) {
			t.Fatal("rejected credential pair rewrote the stored pair")
		}
	}
}

func TestCloudflareSettingsAreWorkspaceScoped(t *testing.T) {
	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", "")
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "")
	ctx := context.Background()
	firstDir, secondDir := t.TempDir(), t.TempDir()
	writeCloudflareConfigTestFile(t, firstDir, cloudflareConfigJSON(""))
	writeCloudflareConfigTestFile(t, secondDir, cloudflareConfigJSON(""))
	first, second := &Service{Dir: firstDir}, &Service{Dir: secondDir}

	if _, err := first.UpdateCloudflareSettings(ctx, CloudflareConfigInput{
		Enabled: true, Endpoint: "https://first.r2.cloudflarestorage.com",
		Bucket: "first-bucket", PublicBaseURL: "https://first.example.test",
		AccessKeyID: "first-access-SENTINEL", SecretAccessKey: "first-secret-SENTINEL",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := second.UpdateCloudflareSettings(ctx, CloudflareConfigInput{
		Enabled: true, Endpoint: "https://second.r2.cloudflarestorage.com",
		Bucket: "second-bucket", PublicBaseURL: "https://second.example.test",
		AccessKeyID: "second-access-SENTINEL", SecretAccessKey: "second-secret-SENTINEL",
	}); err != nil {
		t.Fatal(err)
	}
	firstView, err := first.CloudflareSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	secondView, err := second.CloudflareSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if firstView.Bucket != "first-bucket" || secondView.Bucket != "second-bucket" {
		t.Fatalf("settings crossed workspaces: %+v %+v", firstView, secondView)
	}
	firstPair, err := credential.ResolveR2(ctx, firstDir)
	if err != nil || firstPair.AccessKeyID != "first-access-SENTINEL" {
		t.Fatalf("first workspace pair = %+v, %v", firstPair, err)
	}
	secondPair, err := credential.ResolveR2(ctx, secondDir)
	if err != nil || secondPair.AccessKeyID != "second-access-SENTINEL" {
		t.Fatalf("second workspace pair = %+v, %v", secondPair, err)
	}
	if _, err := os.Stat(filepath.Join(secondDir, "cloudflare", "credentials.json")); err != nil {
		t.Fatalf("second workspace credential file missing: %v", err)
	}
}

func TestCloudflareSettingsRejectInvalidInputWithoutWriting(t *testing.T) {
	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", "")
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "")
	for _, tc := range []struct {
		name  string
		input CloudflareConfigInput
	}{
		{"enabled without endpoint", CloudflareConfigInput{Enabled: true, Bucket: "touko-images", PublicBaseURL: "https://img.example.test"}},
		{"http endpoint", CloudflareConfigInput{Enabled: true, Endpoint: "http://acct.r2.cloudflarestorage.com", Bucket: "touko-images", PublicBaseURL: "https://img.example.test"}},
		{"endpoint userinfo", CloudflareConfigInput{Enabled: true, Endpoint: "https://user:hunter2@acct.r2.cloudflarestorage.com", Bucket: "touko-images", PublicBaseURL: "https://img.example.test"}},
		{"endpoint path", CloudflareConfigInput{Enabled: true, Endpoint: "https://acct.r2.cloudflarestorage.com/bucket", Bucket: "touko-images", PublicBaseURL: "https://img.example.test"}},
		{"bucket separator", CloudflareConfigInput{Enabled: true, Endpoint: "https://acct.r2.cloudflarestorage.com", Bucket: "touko/images", PublicBaseURL: "https://img.example.test"}},
		{"http public base", CloudflareConfigInput{Enabled: true, Endpoint: "https://acct.r2.cloudflarestorage.com", Bucket: "touko-images", PublicBaseURL: "http://img.example.test"}},
		{"public base traversal", CloudflareConfigInput{Enabled: true, Endpoint: "https://acct.r2.cloudflarestorage.com", Bucket: "touko-images", PublicBaseURL: "https://img.example.test/%2e%2e/secret"}},
		{"invalid credential value", CloudflareConfigInput{AccessKeyID: "access SENTINEL", SecretAccessKey: "secret-SENTINEL"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			content := cloudflareConfigJSON("")
			writeCloudflareConfigTestFile(t, dir, content)
			_, err := (&Service{Dir: dir}).UpdateCloudflareSettings(context.Background(), tc.input)
			var settings *CloudflareConfigError
			if !errors.As(err, &settings) || settings.Code != "configuration_invalid" || settings.Message == "" {
				t.Fatalf("invalid settings error = %v", err)
			}
			if strings.Contains(err.Error(), "SENTINEL") || strings.Contains(err.Error(), "hunter2") {
				t.Fatal("settings error disclosed a value")
			}
			data, readErr := os.ReadFile(filepath.Join(dir, "config.json"))
			if readErr != nil || string(data) != content {
				t.Fatal("rejected settings rewrote config.json")
			}
			if _, statErr := os.Stat(filepath.Join(dir, "cloudflare")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("rejected settings created credential state: %v", statErr)
			}
		})
	}
}

func TestCloudflareSettingsAllowDisablingInertValues(t *testing.T) {
	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", "")
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "")
	dir := t.TempDir()
	writeCloudflareConfigTestFile(t, dir, cloudflareConfigJSON(`"cloudflare":{"enabled":true,"endpoint":"http://old.example.test","bucket":"bad/bucket","public_base_url":"http://img.example.test"}`))
	service := &Service{Dir: dir}
	ctx := context.Background()
	inert := CloudflareConfigInput{
		Endpoint: "http://old.example.test", Bucket: "bad/bucket", PublicBaseURL: "http://img.example.test",
	}

	// Turning the mirror off never fails because the stored target is invalid;
	// the values stay visible so they can be corrected later.
	view, err := service.UpdateCloudflareSettings(ctx, CloudflareConfigInput{
		Enabled: false, Endpoint: inert.Endpoint, Bucket: inert.Bucket, PublicBaseURL: inert.PublicBaseURL,
	})
	if err != nil {
		t.Fatalf("disabling invalid stored target failed: %v", err)
	}
	if view.Enabled || view.Endpoint != inert.Endpoint || view.Bucket != inert.Bucket {
		t.Fatalf("disabled settings = %+v", view)
	}
	cfg, err := service.CloudflareConfig(ctx)
	if err != nil || cfg != (r2.Config{}) {
		t.Fatalf("disabled invalid configuration became effective: %+v, %v", cfg, err)
	}

	// Enabling those values is rejected.
	_, err = service.UpdateCloudflareSettings(ctx, CloudflareConfigInput{
		Enabled: true, Endpoint: inert.Endpoint, Bucket: inert.Bucket, PublicBaseURL: inert.PublicBaseURL,
	})
	var settings *CloudflareConfigError
	if !errors.As(err, &settings) || settings.Code != "configuration_invalid" {
		t.Fatalf("enabling invalid target = %v", err)
	}
}

func TestCloudflareSettingsWithoutConfigFile(t *testing.T) {
	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", "")
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "")
	dir := t.TempDir()
	service := &Service{Dir: dir}
	view, err := service.CloudflareSettings(context.Background())
	if err != nil || view.Enabled || view.Endpoint != "" || view.CredentialsConfigured {
		t.Fatalf("empty settings view = %+v, %v", view, err)
	}
	_, err = service.UpdateCloudflareSettings(context.Background(), CloudflareConfigInput{Enabled: false})
	var settings *CloudflareConfigError
	if !errors.As(err, &settings) || !strings.Contains(settings.Message, "select a repository") {
		t.Fatalf("settings write without config = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "config.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed settings write created config.json: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "cloudflare")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed settings write created credential state: %v", statErr)
	}
}
