package r2

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode"

	"github.com/Eurekaimer/TOUKO/internal/delivery"
)

// Config is the optional Cloudflare R2 mirror configuration embedded in the
// workspace config as an independent "cloudflare" object.
type Config struct {
	Enabled       bool   `json:"enabled"`
	Endpoint      string `json:"endpoint"`
	Bucket        string `json:"bucket"`
	PublicBaseURL string `json:"public_base_url"`
}

func configurationInvalid(cause error) error {
	return &Error{Op: "config", Code: "configuration_invalid", Cause: cause}
}

// Validate checks the configured fields. It never contacts Cloudflare and
// never reads credentials. Region selection is fixed to "auto".
func (c Config) Validate() error {
	if _, err := parseEndpoint(c.Endpoint); err != nil {
		return configurationInvalid(err)
	}
	if err := validateBucket(c.Bucket); err != nil {
		return configurationInvalid(err)
	}
	if err := delivery.ValidateCloudflareBaseURL(c.PublicBaseURL); err != nil {
		return configurationInvalid(err)
	}
	return nil
}

// NormalizedEndpoint is the target-identity form of the configured S3
// endpoint: lowercase scheme and host with no root path. It returns an empty
// string for an invalid endpoint, so callers must validate first.
func (c Config) NormalizedEndpoint() string {
	u, err := parseEndpoint(c.Endpoint)
	if err != nil {
		return ""
	}
	return "https://" + strings.ToLower(u.Host)
}

// TargetID identifies a mirror target by normalized endpoint and bucket.
// public_base_url and credentials deliberately do not participate.
func (c Config) TargetID() string {
	raw, err := json.Marshal([]string{c.NormalizedEndpoint(), c.Bucket})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func parseEndpoint(raw string) (*url.URL, error) {
	invalid := fmt.Errorf("invalid Cloudflare R2 endpoint")
	if raw == "" || strings.ContainsRune(raw, '#') {
		return nil, invalid
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, invalid
	}
	if !strings.EqualFold(u.Scheme, "https") || u.Opaque != "" || u.Host == "" || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" ||
		strings.ContainsAny(u.Host, " \t\r\n\\") {
		return nil, invalid
	}
	if u.Path != "" && u.Path != "/" {
		return nil, invalid
	}
	return u, nil
}

func validateBucket(bucket string) error {
	invalid := fmt.Errorf("invalid Cloudflare R2 bucket")
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
