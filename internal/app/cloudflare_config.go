package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/Eurekaimer/TOUKO/internal/atomicfile"
	"github.com/Eurekaimer/TOUKO/internal/credential"
	"github.com/Eurekaimer/TOUKO/internal/delivery"
	"github.com/Eurekaimer/TOUKO/internal/r2"
)

// CloudflareConfigView is the secret-free settings state the CLI and Web UI may
// see. Credentials are represented only by whether a usable pair is available.
type CloudflareConfigView struct {
	Enabled               bool   `json:"enabled"`
	Endpoint              string `json:"endpoint"`
	Bucket                string `json:"bucket"`
	PublicBaseURL         string `json:"public_base_url"`
	CredentialsConfigured bool   `json:"credentials_configured"`
}

// CloudflareConfigInput is a complete settings update. Empty AccessKeyID and
// SecretAccessKey keep the stored pair; supplying exactly one is rejected.
type CloudflareConfigInput struct {
	Enabled         bool
	Endpoint        string
	Bucket          string
	PublicBaseURL   string
	AccessKeyID     string
	SecretAccessKey string
}

// CloudflareCredentialsView is the explicit settings-autofill response. It is
// the only API value that carries the effective R2 pair; callers must return it
// only on the same-origin, CSRF-protected credentials endpoint. Source is
// "saved" for this workspace's private file, "environment" for a complete
// process pair, or "none" when neither is configured.
type CloudflareCredentialsView struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Source          string `json:"source"`
}

// CloudflareConfigError reports a rejected settings read or write. Message is
// controlled text authored by TOUKO; it never contains credential values.
type CloudflareConfigError struct {
	Code    string
	Message string
}

func (e *CloudflareConfigError) Error() string { return e.Message }

func cloudflareSettingsInvalid(message string) error {
	return &CloudflareConfigError{Code: cloudflareCodeConfigurationInvalid, Message: message}
}

// CloudflareConfig reads only the optional cloudflare object from the existing
// workspace config. The legacy location fields stay on their original loose
// decoding path and are not rewritten here.
func (s *Service) CloudflareConfig(ctx context.Context) (r2.Config, error) {
	cfg, err := s.readCloudflareConfig(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return r2.Config{}, err
		}
		// Mirror paths classify failures through the R2 error contract; the
		// settings UI keeps the friendlier CloudflareConfigError from the read.
		return r2.Config{}, cloudflareConfigInvalid(err)
	}
	if !cfg.Enabled {
		return r2.Config{}, nil
	}
	if err := cfg.Validate(); err != nil {
		return r2.Config{}, cloudflareConfigInvalid(err)
	}
	return cfg, nil
}

// CloudflareSettings returns the stored block, including the fields of a
// disabled mirror, plus the workspace-scoped credential readiness. It never
// fails because the mirror is disabled or credentials are missing.
func (s *Service) CloudflareSettings(ctx context.Context) (CloudflareConfigView, error) {
	cfg, err := s.readCloudflareConfig(ctx)
	if err != nil {
		return CloudflareConfigView{}, err
	}
	return s.cloudflareConfigView(ctx, cfg), nil
}

// CloudflareCredentials resolves the pair used by settings autofill. A saved
// workspace pair wins; a complete, valid environment pair is used only when
// this workspace has no saved file. The two sources are never mixed, and a
// partial or invalid environment is treated as unconfigured. Errors from an
// unreadable saved file are controlled and never include either value.
func (s *Service) CloudflareCredentials(ctx context.Context) (CloudflareCredentialsView, error) {
	if err := ctx.Err(); err != nil {
		return CloudflareCredentialsView{}, err
	}
	pair, found, err := credential.LoadR2(ctx, s.Dir)
	if err != nil {
		return CloudflareCredentialsView{}, err
	}
	if found {
		return CloudflareCredentialsView{
			AccessKeyID:     pair.AccessKeyID,
			SecretAccessKey: pair.SecretAccessKey,
			Source:          "saved",
		}, nil
	}
	pair, found, err = credential.EnvironmentR2()
	if err != nil || !found {
		return CloudflareCredentialsView{Source: "none"}, nil
	}
	return CloudflareCredentialsView{
		AccessKeyID:     pair.AccessKeyID,
		SecretAccessKey: pair.SecretAccessKey,
		Source:          "environment",
	}, nil
}

// UpdateCloudflareSettings durably writes the optional cloudflare block while
// preserving every other config.json field as a JSON value. It never contacts
// Cloudflare, syncs or uploads: saving settings only changes local state.
func (s *Service) UpdateCloudflareSettings(ctx context.Context, in CloudflareConfigInput) (CloudflareConfigView, error) {
	if err := ctx.Err(); err != nil {
		return CloudflareConfigView{}, err
	}
	cfg := r2.Config{
		Enabled:       in.Enabled,
		Endpoint:      in.Endpoint,
		Bucket:        in.Bucket,
		PublicBaseURL: in.PublicBaseURL,
	}
	if err := validateCloudflareSettings(cfg); err != nil {
		return CloudflareConfigView{}, err
	}
	hasAccess := in.AccessKeyID != ""
	hasSecret := in.SecretAccessKey != ""
	if hasAccess != hasSecret {
		return CloudflareConfigView{}, cloudflareSettingsInvalid("provide both the access key id and the secret access key, or leave both empty to keep the saved credentials")
	}
	pair := credential.R2Pair{AccessKeyID: in.AccessKeyID, SecretAccessKey: in.SecretAccessKey}
	if hasAccess {
		if err := credential.ValidateR2Pair(pair); err != nil {
			return CloudflareConfigView{}, cloudflareSettingsInvalid(err.Error())
		}
	}
	unlock, err := s.Lock(ctx)
	if err != nil {
		return CloudflareConfigView{}, err
	}
	defer unlock()
	fields, err := s.readConfigFields(ctx)
	if err != nil {
		return CloudflareConfigView{}, err
	}
	// Credentials are replaced first: if that write fails the validated config
	// file is untouched, so the mirror cannot become enabled with a half-saved
	// pair. Config replacement remains the single commit point for the block.
	if hasAccess {
		if err := credential.SaveR2(ctx, s.Dir, pair); err != nil {
			return CloudflareConfigView{}, cloudflareSettingsInvalid("the Cloudflare R2 credentials could not be saved in this workspace: " + err.Error())
		}
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return CloudflareConfigView{}, err
	}
	fields["cloudflare"] = json.RawMessage(data)
	encoded, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return CloudflareConfigView{}, err
	}
	if err := atomicfile.Write(ctx, filepath.Join(s.Dir, "config.json"), append(encoded, '\n'), 0600); err != nil {
		return CloudflareConfigView{}, cloudflareSettingsInvalid("the workspace configuration could not be written: " + err.Error())
	}
	return s.cloudflareConfigView(ctx, cfg), nil
}

// readCloudflareConfig strictly decodes only the cloudflare object. The outer
// document keeps its historical loose decoding so unrelated fields, including
// the legacy location, never become a compatibility requirement here.
func (s *Service) readCloudflareConfig(ctx context.Context) (r2.Config, error) {
	if err := ctx.Err(); err != nil {
		return r2.Config{}, err
	}
	b, err := os.ReadFile(filepath.Join(s.Dir, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return r2.Config{}, nil
	}
	if err != nil {
		return r2.Config{}, cloudflareSettingsInvalid("the workspace configuration is unreadable")
	}
	var envelope struct {
		Cloudflare json.RawMessage `json:"cloudflare"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil {
		return r2.Config{}, cloudflareSettingsInvalid("the workspace configuration is not a JSON object")
	}
	raw := bytes.TrimSpace(envelope.Cloudflare)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return r2.Config{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var cfg r2.Config
	if err := decoder.Decode(&cfg); err != nil {
		return r2.Config{}, cloudflareSettingsInvalid("the cloudflare block must contain only enabled, endpoint, bucket and public_base_url")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return r2.Config{}, cloudflareSettingsInvalid("the cloudflare block must be exactly one JSON object")
	}
	return cfg, nil
}

// readConfigFields keeps every top-level value as raw JSON so saving settings
// cannot drop or rewrite the legacy location, appearance or future fields.
func (s *Service) readConfigFields(ctx context.Context) (map[string]json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(s.Dir, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, cloudflareSettingsInvalid("select a repository before saving Cloudflare settings")
	}
	if err != nil {
		return nil, cloudflareSettingsInvalid("the workspace configuration is unreadable")
	}
	fields := make(map[string]json.RawMessage)
	if err := json.Unmarshal(b, &fields); err != nil {
		return nil, cloudflareSettingsInvalid("the workspace configuration is not a JSON object")
	}
	return fields, nil
}

func (s *Service) cloudflareConfigView(ctx context.Context, cfg r2.Config) CloudflareConfigView {
	return CloudflareConfigView{
		Enabled:               cfg.Enabled,
		Endpoint:              cfg.Endpoint,
		Bucket:                cfg.Bucket,
		PublicBaseURL:         cfg.PublicBaseURL,
		CredentialsConfigured: s.cloudflareCredentialsConfigured(ctx),
	}
}

func (s *Service) cloudflareCredentialsConfigured(ctx context.Context) bool {
	_, err := credential.ResolveR2(ctx, s.Dir)
	return err == nil
}

// validateCloudflareSettings only enforces the target shape for an enabled
// mirror. A disabled block is inert and may keep legacy or partially edited
// values, so turning the mirror off always succeeds; enabling it validates
// every field, and an unsafe value never becomes active.
func validateCloudflareSettings(cfg r2.Config) error {
	if !cfg.Enabled {
		return nil
	}
	if cfg.Endpoint == "" {
		return cloudflareSettingsInvalid("an endpoint is required when the Cloudflare mirror is enabled")
	}
	if err := r2.ValidateEndpoint(cfg.Endpoint); err != nil {
		return cloudflareSettingsInvalid("the endpoint must be an absolute HTTPS URL without user info, query, fragment or path")
	}
	if cfg.Bucket == "" {
		return cloudflareSettingsInvalid("a bucket is required when the Cloudflare mirror is enabled")
	}
	if err := r2.ValidateBucket(cfg.Bucket); err != nil {
		return cloudflareSettingsInvalid("the bucket must not contain slashes, whitespace or control characters")
	}
	if cfg.PublicBaseURL == "" {
		return cloudflareSettingsInvalid("a public_base_url is required when the Cloudflare mirror is enabled")
	}
	if err := delivery.ValidateCloudflareBaseURL(cfg.PublicBaseURL); err != nil {
		return cloudflareSettingsInvalid("public_base_url must be an HTTPS URL without user info, query or fragment, with a safe optional path prefix")
	}
	return nil
}

func cloudflareConfigInvalid(cause error) error {
	var target *r2.Error
	if errors.As(cause, &target) {
		return cause
	}
	return &r2.Error{Op: "config", Code: "configuration_invalid", Cause: cause}
}
