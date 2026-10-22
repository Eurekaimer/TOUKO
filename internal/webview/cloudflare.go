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
