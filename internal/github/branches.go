package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// BranchInfo describes a branch, including whether GitHub protects it.
type BranchInfo struct {
	Name      string `json:"name"`
	Protected bool   `json:"protected"`
}

// ListBranches returns every branch, including protected and read-only branches.
// Public repositories may be read without credentials.
func (r *Repository) ListBranches(ctx context.Context) ([]BranchInfo, error) {
	result := []BranchInfo{}
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("%s/branches?per_page=100&page=%d", r.path(), page)
		response, err := r.request(ctx, http.MethodGet, endpoint, jsonAccept, nil, 0)
		if err != nil {
			return nil, err
		}
		var entries []BranchInfo
		dec := json.NewDecoder(io.LimitReader(response.Body, (64<<20)+1))
		err = dec.Decode(&entries)
		if err == nil {
			if trailing := dec.Decode(new(any)); !errors.Is(trailing, io.EOF) {
				err = fmt.Errorf("trailing or oversized JSON")
			}
		}
		closeErr := response.Body.Close()
		if err != nil {
			return nil, r.safeError(fmt.Errorf("GitHub branch list: invalid JSON: %w", err))
		}
		if closeErr != nil {
			return nil, r.safeError(closeErr)
		}
		result = append(result, entries...)
		// Use Link only as a pagination signal, never as a credential-bearing
		// request target. All pages stay on the configured, trusted API origin.
		hasNext := false
		for _, link := range strings.Split(response.Header.Get("Link"), ",") {
			if strings.Contains(link, `rel="next"`) {
				hasNext = true
			}
		}
		if !hasNext && len(entries) < 100 {
			return result, nil
		}
	}
}
