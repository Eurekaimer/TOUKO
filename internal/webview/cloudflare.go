package webview

import (
	"net/http"

	"github.com/Eurekaimer/TOUKO/internal/app"
)

// cloudflareSync mirrors the authoritative GitHub snapshot. The request body is
// exactly {"dry_run":bool}; no endpoint, bucket or credential is accepted from
// the browser. lazyGit keeps the same GitHub API backend as other writes while
// deferring credential resolution until a configured mirror needs HEAD.
func (s *Server) cloudflareSync(w http.ResponseWriter, r *http.Request) (any, error) {
	var input struct {
		DryRun bool `json:"dry_run"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		return nil, err
	}
	return s.service.CloudflareSync(r.Context(), &lazyGit{server: s}, app.CloudflareOptions{DryRun: input.DryRun})
}

// cloudflareCredentials returns this member's effective R2 pair for masked
// settings autofill. The body must be exactly {}; the route is POST-only and
// inherits the exact Host/Origin/CSRF, no-store and workMu protections. A saved
// workspace pair wins over a complete environment pair, and neither source is
// mixed. This is the only response that intentionally carries the values.
func (s *Server) cloudflareCredentials(w http.ResponseWriter, r *http.Request) (any, error) {
	if err := decodeJSON(w, r, &struct{}{}); err != nil {
		return nil, err
	}
	return s.service.CloudflareCredentials(r.Context())
}

// cloudflareConfig reads or replaces only this member's optional Cloudflare
// settings. GET never returns credentials, only whether a usable pair exists.
// POST accepts credentials but stores them in the workspace's private 0600
// credential file, never in config.json, and never echoes them back. Saving is
// local only: it does not enable a sync, upload or GitHub operation.
func (s *Server) cloudflareConfig(w http.ResponseWriter, r *http.Request) (any, error) {
	if r.Method == http.MethodGet {
		return s.service.CloudflareSettings(r.Context())
	}
	var input struct {
		Enabled         bool   `json:"enabled"`
		Endpoint        string `json:"endpoint"`
		Bucket          string `json:"bucket"`
		PublicBaseURL   string `json:"public_base_url"`
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		return nil, err
	}
	return s.service.UpdateCloudflareSettings(r.Context(), app.CloudflareConfigInput{
		Enabled:         input.Enabled,
		Endpoint:        input.Endpoint,
		Bucket:          input.Bucket,
		PublicBaseURL:   input.PublicBaseURL,
		AccessKeyID:     input.AccessKeyID,
		SecretAccessKey: input.SecretAccessKey,
	})
}
