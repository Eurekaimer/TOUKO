package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/Eurekaimer/TOUKO/internal/r2"
)

// CloudflareConfig reads only the optional cloudflare object from the existing
// workspace config. The legacy location fields stay on their original loose
// decoding path and are not rewritten here.
func (s *Service) CloudflareConfig(ctx context.Context) (r2.Config, error) {
	if err := ctx.Err(); err != nil {
		return r2.Config{}, err
	}
	b, err := os.ReadFile(filepath.Join(s.Dir, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return r2.Config{}, nil
	}
	if err != nil {
		return r2.Config{}, cloudflareConfigInvalid(err)
	}
	var envelope struct {
		Cloudflare json.RawMessage `json:"cloudflare"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil {
		return r2.Config{}, cloudflareConfigInvalid(err)
	}
	raw := bytes.TrimSpace(envelope.Cloudflare)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return r2.Config{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var cfg r2.Config
	if err := decoder.Decode(&cfg); err != nil {
		return r2.Config{}, cloudflareConfigInvalid(err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("trailing JSON after cloudflare configuration")
		}
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

func cloudflareConfigInvalid(cause error) error {
	var target *r2.Error
	if errors.As(cause, &target) {
		return cause
	}
	return &r2.Error{Op: "config", Code: "configuration_invalid", Cause: cause}
}
