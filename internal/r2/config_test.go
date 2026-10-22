package r2_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/r2"
)

func validR2Config() r2.Config {
	return r2.Config{
		Enabled:       true,
		Endpoint:      "https://acct.r2.cloudflarestorage.com",
		Bucket:        "touko-images",
		PublicBaseURL: "https://img.eurekaimer.icu",
	}
}

func assertR2ConfigurationInvalid(t *testing.T, err error) *r2.Error {
	t.Helper()
	if err == nil {
		t.Fatal("expected a configuration error")
	}
	if code := r2.ErrorCode(err); code != "configuration_invalid" {
		t.Fatalf("error code = %q; want configuration_invalid (%v)", code, err)
	}
	var target *r2.Error
	if !errors.As(err, &target) {
		t.Fatalf("error is not an *r2.Error: %v", err)
	}
	if target.Op != "config" {
		t.Fatalf("error op = %q; want config", target.Op)
	}
	return target
}

func TestConfigValidateAcceptsSupportedConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  r2.Config
	}{
		{"standard endpoint", validR2Config()},
		{"jurisdiction endpoint", func() r2.Config {
			cfg := validR2Config()
			cfg.Endpoint = "https://acct.eu.r2.cloudflarestorage.com/"
			return cfg
		}()},
		{"uppercase endpoint", func() r2.Config {
			cfg := validR2Config()
			cfg.Endpoint = "HTTPS://ACCT.R2.CLOUDFLARESTORAGE.COM"
			return cfg
		}()},
		{"loopback TLS fixture", func() r2.Config {
			cfg := validR2Config()
			cfg.Endpoint = "https://127.0.0.1:8443"
			return cfg
		}()},
		{"escaped public base prefix", func() r2.Config {
			cfg := validR2Config()
			cfg.PublicBaseURL = "https://img.example.com/my%20pics/%E5%9B%BE/"
			return cfg
		}()},
		{"public base without trailing slash", func() r2.Config {
			cfg := validR2Config()
			cfg.PublicBaseURL = "https://img.example.com/images"
			return cfg
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(); err != nil {
				t.Fatalf("Validate(%+v) = %v", tc.cfg, err)
			}
		})
	}
}

func TestConfigValidateRejectsInvalidEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"",
		"acct.r2.cloudflarestorage.com",
		"http://acct.r2.cloudflarestorage.com",
		"ftp://acct.r2.cloudflarestorage.com",
		"https://",
		"https://user:pass@acct.r2.cloudflarestorage.com",
		"https://acct.r2.cloudflarestorage.com/bucket",
		"https://acct.r2.cloudflarestorage.com/?x=1",
		"https://acct.r2.cloudflarestorage.com/#frag",
		"https://acct.r2.cloudflarestorage.com//",
		"https://acct.r2.cloudflarestorage.com/%2e%2e",
		"https:opaque",
	} {
		t.Run(endpoint, func(t *testing.T) {
			cfg := validR2Config()
			cfg.Endpoint = endpoint
			assertR2ConfigurationInvalid(t, cfg.Validate())
		})
	}
}

func TestConfigValidateRejectsInvalidBucket(t *testing.T) {
	for _, bucket := range []string{
		"",
		"touko/images",
		`touko\images`,
		"touko images",
		"touko\timages",
		"touko\nimages",
		"touko\u00a0images",
		"touko\x00images",
	} {
		t.Run(bucket, func(t *testing.T) {
			cfg := validR2Config()
			cfg.Bucket = bucket
			assertR2ConfigurationInvalid(t, cfg.Validate())
		})
	}
}

func TestConfigValidateRejectsInvalidPublicBaseURL(t *testing.T) {
	for _, base := range []string{
		"",
		"img.example.com",
		"http://img.example.com",
		"https://img.example.com?x=1",
		"https://img.example.com/#frag",
		"https://user:pass@img.example.com",
		"https://img.example.com/../secret",
		"https://img.example.com/%2e%2e/secret",
		"https://img.example.com/a%2F..%2Fsecret",
		"https://img.example.com/a//b",
		"https://img.example.com/a/./b",
		"https:opaque",
	} {
		t.Run(base, func(t *testing.T) {
			cfg := validR2Config()
			cfg.PublicBaseURL = base
			assertR2ConfigurationInvalid(t, cfg.Validate())
		})
	}
}

func TestConfigValidateErrorsDoNotDiscloseEndpointCredentials(t *testing.T) {
	cfg := validR2Config()
	cfg.Endpoint = "https://user:hunter2@acct.r2.cloudflarestorage.com"
	err := cfg.Validate()
	target := assertR2ConfigurationInvalid(t, err)
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(target.Cause.Error(), "hunter2") {
		t.Fatal("configuration error disclosed endpoint credentials")
	}
}

func TestConfigTargetIdentity(t *testing.T) {
	cfg := validR2Config()
	cfg.Endpoint = "HTTPS://Account.R2.CloudflareStorage.com/"
	if got := cfg.NormalizedEndpoint(); got != "https://account.r2.cloudflarestorage.com" {
		t.Fatalf("NormalizedEndpoint() = %q", got)
	}
	sum := sha256.Sum256([]byte(`["https://account.r2.cloudflarestorage.com","touko-images"]`))
	wantID := hex.EncodeToString(sum[:])
	if got := cfg.TargetID(); got != wantID {
		t.Fatalf("TargetID() = %q; want %q", got, wantID)
	}
	if len(wantID) != 64 || strings.ToLower(wantID) != wantID {
		t.Fatalf("target identity is not 64 lowercase hex characters: %q", wantID)
	}

	renamed := cfg
	renamed.PublicBaseURL = "https://img.other.example"
	if renamed.TargetID() != wantID {
		t.Fatal("changing public_base_url changed the target identity")
	}
	otherBucket := cfg
	otherBucket.Bucket = "other-bucket"
	if otherBucket.TargetID() == wantID {
		t.Fatal("changing bucket kept the target identity")
	}
	otherEndpoint := cfg
	otherEndpoint.Endpoint = "https://acct.eu.r2.cloudflarestorage.com"
	if otherEndpoint.TargetID() == wantID {
		t.Fatal("changing endpoint kept the target identity")
	}
}

func TestConfigTargetIdentityNormalizesTrailingSlashAndCaseOnly(t *testing.T) {
	first := validR2Config()
	first.Endpoint = "https://ACCT.r2.cloudflarestorage.com/"
	second := validR2Config()
	second.Endpoint = "https://acct.r2.cloudflarestorage.com"
	if first.NormalizedEndpoint() != second.NormalizedEndpoint() {
		t.Fatalf("endpoint normalization differs: %q != %q", first.NormalizedEndpoint(), second.NormalizedEndpoint())
	}
	if first.TargetID() != second.TargetID() {
		t.Fatal("equivalent endpoints produced different target identities")
	}
}
