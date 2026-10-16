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

// RepositoryInfo describes a repository accessible to the authenticated account.
type RepositoryInfo struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	CanPush       bool   `json:"can_push"`
}

// ListRepositories returns all accessible repositories, not just the first page.
func (r *Repository) ListRepositories(ctx context.Context) ([]RepositoryInfo, error) {
	if err := r.writeAllowed(); err != nil {
		return nil, err
	}
	result := []RepositoryInfo{}
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("/user/repos?per_page=100&page=%d&affiliation=owner,collaborator,organization_member&sort=full_name&direction=asc", page)
		response, err := r.request(ctx, http.MethodGet, endpoint, jsonAccept, nil, 0)
		if err != nil {
			return nil, err
		}
		var entries []struct {
			FullName      string `json:"full_name"`
			DefaultBranch string `json:"default_branch"`
			Private       bool   `json:"private"`
			Permissions   struct {
				Push bool `json:"push"`
			} `json:"permissions"`
		}
		dec := json.NewDecoder(io.LimitReader(response.Body, (64<<20)+1))
		err = dec.Decode(&entries)
		if err == nil {
			if trailing := dec.Decode(new(any)); !errors.Is(trailing, io.EOF) {
				err = fmt.Errorf("trailing or oversized JSON")
			}
		}
		closeErr := response.Body.Close()
		if err != nil {
			return nil, r.safeError(fmt.Errorf("GitHub repository list: invalid JSON: %w", err))
		}
		if closeErr != nil {
			return nil, r.safeError(closeErr)
		}
		for _, entry := range entries {
			result = append(result, RepositoryInfo{FullName: entry.FullName, DefaultBranch: entry.DefaultBranch, Private: entry.Private, CanPush: entry.Permissions.Push})
		}
		// Follow pagination only on our trusted API origin, never a Link URL that
		// could forward the bearer credential to another host.
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
