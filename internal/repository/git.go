package repository

import (
	"context"
	"github.com/Eurekaimer/TOUKO/internal/domain"
	"io"
)

type BlobID string
type TreeID string
type GitCommit struct {
	Tree    TreeID
	Parents []domain.RepositoryHead
}
type GitEntry struct {
	Path, Mode, Type, SHA string
	Size                  int64
}
type TreeChange struct {
	Path string
	Blob *BlobID
}
type Reader interface {
	Head(context.Context) (domain.RepositoryHead, error)
	Commit(context.Context, domain.RepositoryHead) (GitCommit, error)
	Tree(context.Context, TreeID) ([]GitEntry, error)
	OpenBlob(context.Context, BlobID) (io.ReadCloser, error)
	IsAncestor(context.Context, domain.RepositoryHead, domain.RepositoryHead) (bool, error)
}
type Writer interface {
	CreateBlob(context.Context, io.Reader, int64) (BlobID, error)
	CreateTree(context.Context, TreeID, []TreeChange) (TreeID, error)
	CreateCommit(context.Context, TreeID, domain.RepositoryHead, string) (domain.RepositoryHead, error)
	UpdateRef(context.Context, domain.RepositoryHead, domain.RepositoryHead) error
}
type Git interface {
	Reader
	Writer
}
