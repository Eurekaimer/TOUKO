package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

func validID(id string) bool {
	if len(id) != 40 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func checkID(id string) error {
	if !validID(id) {
		return fmt.Errorf("invalid Git object ID")
	}
	return nil
}
func responseID(id string) error {
	if !validID(id) {
		return fmt.Errorf("%w: GitHub returned an invalid Git object ID", domain.ErrRepositoryCorrupt)
	}
	return nil
}

func (r *Repository) Head(ctx context.Context) (domain.RepositoryHead, error) {
	var result struct {
		Object struct{ SHA, Type string } `json:"object"`
	}
	err := r.json(ctx, http.MethodGet, r.path()+"/git/ref/heads/"+escapeBranch(r.location.Branch), nil, &result)
	if err != nil {
		var api *APIError
		if errors.As(err, &api) && (api.Status == 404 || api.Status == 409) {
			return "", fmt.Errorf("GitHub branch is missing or repository is empty; initialize the repository with a README on GitHub and select an existing branch: %w", err)
		}
		return "", err
	}
	if result.Object.Type != "commit" {
		return "", fmt.Errorf("%w: branch ref is not a commit", domain.ErrRepositoryCorrupt)
	}
	if err := responseID(result.Object.SHA); err != nil {
		return "", err
	}
	return domain.RepositoryHead(result.Object.SHA), nil
}

func (r *Repository) Commit(ctx context.Context, head domain.RepositoryHead) (repository.GitCommit, error) {
	if err := checkID(string(head)); err != nil {
		return repository.GitCommit{}, err
	}
	var result struct {
		Tree    struct{ SHA string }   `json:"tree"`
		Parents []struct{ SHA string } `json:"parents"`
	}
	if err := r.json(ctx, http.MethodGet, r.path()+"/git/commits/"+string(head), nil, &result); err != nil {
		return repository.GitCommit{}, err
	}
	if err := responseID(result.Tree.SHA); err != nil {
		return repository.GitCommit{}, err
	}
	c := repository.GitCommit{Tree: repository.TreeID(result.Tree.SHA), Parents: make([]domain.RepositoryHead, 0, len(result.Parents))}
	for _, p := range result.Parents {
		if err := responseID(p.SHA); err != nil {
			return repository.GitCommit{}, err
		}
		c.Parents = append(c.Parents, domain.RepositoryHead(p.SHA))
	}
	return c, nil
}

func (r *Repository) Tree(ctx context.Context, tree repository.TreeID) ([]repository.GitEntry, error) {
	if err := checkID(string(tree)); err != nil {
		return nil, err
	}
	var result struct {
		Tree []struct {
			Path, Mode, Type, SHA string
			Size                  int64
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	if err := r.json(ctx, http.MethodGet, r.path()+"/git/trees/"+string(tree)+"?recursive=1", nil, &result); err != nil {
		return nil, err
	}
	if result.Truncated {
		return nil, fmt.Errorf("%w: GitHub recursive tree is truncated; refusing an incomplete repository snapshot", domain.ErrRepositoryCorrupt)
	}
	entries := make([]repository.GitEntry, 0, len(result.Tree))
	for _, e := range result.Tree {
		if err := responseID(e.SHA); err != nil {
			return nil, err
		}
		entries = append(entries, repository.GitEntry{Path: e.Path, Mode: e.Mode, Type: e.Type, SHA: e.SHA, Size: e.Size})
	}
	return entries, nil
}

type blobBody struct {
	io.ReadCloser
	r *Repository
}

func (b *blobBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	return n, b.r.safeError(err)
}
func (b *blobBody) Close() error { return b.r.safeError(b.ReadCloser.Close()) }

func (r *Repository) OpenBlob(ctx context.Context, id repository.BlobID) (io.ReadCloser, error) {
	if err := checkID(string(id)); err != nil {
		return nil, err
	}
	response, err := r.request(ctx, http.MethodGet, r.path()+"/git/blobs/"+string(id), rawAccept, nil, 0)
	if err != nil {
		return nil, err
	}
	return &blobBody{ReadCloser: response.Body, r: r}, nil
}

func (r *Repository) IsAncestor(ctx context.Context, ancestor, head domain.RepositoryHead) (bool, error) {
	if err := checkID(string(ancestor)); err != nil {
		return false, err
	}
	if err := checkID(string(head)); err != nil {
		return false, err
	}
	var result struct {
		Status string `json:"status"`
	}
	endpoint := r.path() + "/compare/" + url.PathEscape(string(ancestor)) + "..." + url.PathEscape(string(head)) + "?per_page=1"
	if err := r.json(ctx, http.MethodGet, endpoint, nil, &result); err != nil {
		return false, err
	}
	switch result.Status {
	case "ahead", "identical":
		return true, nil
	case "behind", "diverged":
		return false, nil
	default:
		return false, fmt.Errorf("%w: GitHub compare returned an unknown relationship", domain.ErrRepositoryCorrupt)
	}
}

// CreateBlob streams JSON/base64 through a bounded pipe. Closing the read side
// on every return unblocks the encoder when a server rejects an upload early.
func (r *Repository) CreateBlob(ctx context.Context, source io.Reader, size int64) (repository.BlobID, error) {
	if err := r.writeAllowed(); err != nil {
		return "", err
	}
	if size < 0 || size > 100_000_000 {
		return "", fmt.Errorf("GitHub blob size must be between 0 and 100000000 bytes")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	const prefix = `{"encoding":"base64","content":"`
	const suffix = `"}`
	length := int64(len(prefix)+len(suffix)) + ((size+2)/3)*4
	reader, writer := io.Pipe()
	// Client.Do may wait for its request-body writer even after cancellation.
	// Interrupt the source independently rather than waiting for Do to return.
	uploadCtx, cancel := context.WithTimeout(ctx, r.client.Timeout)
	defer cancel()
	closeSource := sync.OnceFunc(func() {
		if closer, ok := source.(io.Closer); ok {
			_ = closer.Close()
		}
	})
	aborted := make(chan struct{})
	stopAbort := context.AfterFunc(uploadCtx, func() {
		defer close(aborted)
		_ = reader.CloseWithError(uploadCtx.Err())
		_ = writer.CloseWithError(uploadCtx.Err())
		closeSource()
	})
	defer func() {
		if !stopAbort() {
			<-aborted
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, err := io.WriteString(writer, prefix)
		encoder := base64.NewEncoder(base64.StdEncoding, writer)
		if err == nil {
			_, err = io.CopyN(encoder, source, size)
		}
		if err == nil {
			var extra [1]byte
			n, readErr := io.ReadFull(source, extra[:])
			if n != 0 {
				err = fmt.Errorf("blob exceeds declared size")
			} else if readErr != nil && !errors.Is(readErr, io.EOF) {
				err = readErr
			}
		}
		if err == nil {
			err = encoder.Close()
		}
		if err == nil {
			_, err = io.WriteString(writer, suffix)
		}
		_ = writer.CloseWithError(err)
		done <- err
	}()
	response, requestErr := r.request(uploadCtx, http.MethodPost, r.path()+"/git/blobs", jsonAccept, reader, length)
	// HTTP transports may return before draining a rejected request body.
	_ = reader.Close()
	if requestErr != nil {
		// Closing an interruptible source also releases a writer blocked in Read,
		// rather than only the usual writer blocked on the pipe. Successful reads
		// leave source ownership with the caller.
		closeSource()
	}
	streamErr := <-done
	if requestErr != nil {
		// Aborting both pipe ends can make the transport observe ErrClosedPipe
		// before it observes cancellation. Preserve both the transport failure
		// and the context cause so callers can still classify cancellation.
		if cause := uploadCtx.Err(); cause != nil {
			return "", errors.Join(requestErr, cause)
		}
		return "", requestErr
	}
	defer response.Body.Close()
	if streamErr != nil {
		return "", r.safeError(fmt.Errorf("stream GitHub blob: %w", streamErr))
	}
	var result struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return "", r.safeError(err)
	}
	if err := responseID(result.SHA); err != nil {
		return "", err
	}
	return repository.BlobID(result.SHA), nil
}

func (r *Repository) CreateTree(ctx context.Context, base repository.TreeID, changes []repository.TreeChange) (repository.TreeID, error) {
	if err := r.writeAllowed(); err != nil {
		return "", err
	}
	if base != "" {
		if err := checkID(string(base)); err != nil {
			return "", err
		}
	}
	type entry struct {
		Path string             `json:"path"`
		Mode string             `json:"mode"`
		Type string             `json:"type"`
		SHA  *repository.BlobID `json:"sha"`
	}
	input := struct {
		Base string  `json:"base_tree,omitempty"`
		Tree []entry `json:"tree"`
	}{Base: string(base), Tree: make([]entry, 0, len(changes))}
	for _, change := range changes {
		p := change.Path
		if p == "" || path.Clean(p) != p || strings.HasPrefix(p, "/") || p == ".." || strings.HasPrefix(p, "../") || strings.ContainsRune(p, 0) {
			return "", fmt.Errorf("invalid Git tree path")
		}
		if change.Blob != nil {
			if err := checkID(string(*change.Blob)); err != nil {
				return "", err
			}
		}
		input.Tree = append(input.Tree, entry{Path: p, Mode: "100644", Type: "blob", SHA: change.Blob})
	}
	var result struct {
		SHA string `json:"sha"`
	}
	if err := r.json(ctx, http.MethodPost, r.path()+"/git/trees", input, &result); err != nil {
		return "", err
	}
	if err := responseID(result.SHA); err != nil {
		return "", err
	}
	return repository.TreeID(result.SHA), nil
}

func (r *Repository) CreateCommit(ctx context.Context, tree repository.TreeID, parent domain.RepositoryHead, message string) (domain.RepositoryHead, error) {
	if parent == "" {
		return "", fmt.Errorf("GitHub cannot create the first ref of an empty repository through the Git Data API; initialize the repository with a README on GitHub first")
	}
	if err := r.writeAllowed(); err != nil {
		return "", err
	}
	if err := checkID(string(tree)); err != nil {
		return "", err
	}
	if err := checkID(string(parent)); err != nil {
		return "", err
	}
	input := struct {
		Tree    repository.TreeID       `json:"tree"`
		Parents []domain.RepositoryHead `json:"parents"`
		Message string                  `json:"message"`
	}{tree, []domain.RepositoryHead{parent}, message}
	var result struct {
		SHA string `json:"sha"`
	}
	if err := r.json(ctx, http.MethodPost, r.path()+"/git/commits", input, &result); err != nil {
		return "", err
	}
	if err := responseID(result.SHA); err != nil {
		return "", err
	}
	return domain.RepositoryHead(result.SHA), nil
}

func (r *Repository) UpdateRef(ctx context.Context, expected, next domain.RepositoryHead) error {
	if err := r.writeAllowed(); err != nil {
		return err
	}
	if expected == "" {
		return fmt.Errorf("initialize the GitHub repository with a README before publishing")
	}
	if err := checkID(string(expected)); err != nil {
		return err
	}
	if err := checkID(string(next)); err != nil {
		return err
	}
	current, err := r.Head(ctx)
	if err != nil {
		return err
	}
	if current != expected {
		return &domain.ConflictError{Base: expected, Remote: current, Reason: "head_changed"}
	}
	input := struct {
		SHA   domain.RepositoryHead `json:"sha"`
		Force bool                  `json:"force"`
	}{SHA: next, Force: false}
	err = r.json(ctx, http.MethodPatch, r.path()+"/git/refs/heads/"+escapeBranch(r.location.Branch), input, nil)
	if err == nil {
		return nil
	}
	var api *APIError
	if errors.As(err, &api) && (api.Status == 409 || api.Status == 422) {
		current, readErr := r.Head(ctx)
		if readErr == nil && current != expected {
			return &domain.ConflictError{Base: expected, Remote: current, Reason: "head_changed"}
		}
	}
	return err
}
