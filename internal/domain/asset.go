package domain

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
)

type Namespace string

var namespacePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

func ParseNamespace(s string) (Namespace, error) {
	if s == "" {
		s = "default"
	}
	if !namespacePattern.MatchString(s) {
		return "", fmt.Errorf("invalid namespace %q: use a single lowercase ASCII slug", s)
	}
	return Namespace(s), nil
}

type Repository struct {
	FormatVersion int          `json:"format_version"`
	ID            RepositoryID `json:"repository_id"`
	HashAlgorithm string       `json:"hash_algorithm"`
}
type ContentObject struct {
	Hash   ObjectHash `json:"hash"`
	MIME   string     `json:"mime"`
	Ext    string     `json:"ext"`
	Size   int64      `json:"size"`
	Width  int        `json:"width"`
	Height int        `json:"height"`
}
type Asset struct {
	ID           AssetID       `json:"id"`
	Namespace    Namespace     `json:"namespace"`
	Object       ContentObject `json:"object"`
	Path         string        `json:"path,omitempty"`
	OriginalName string        `json:"original_name"`
	Title        string        `json:"title"`
	Tags         []string      `json:"tags"`
	Collections  []string      `json:"collections"`
	CreatedAt    time.Time     `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
}

func (o ContentObject) Validate() error {
	mime := map[string]string{"png": "image/png", "jpg": "image/jpeg", "gif": "image/gif"}[o.Ext]
	if mime == "" || mime != o.MIME || o.Size <= 0 || o.Size > 100_000_000 || o.Width <= 0 || o.Height <= 0 {
		return fmt.Errorf("invalid content object description")
	}
	return nil
}
func (a Asset) Validate() error {
	if a.ID == (AssetID{}) || !namespacePattern.MatchString(string(a.Namespace)) || a.CreatedAt.IsZero() || a.UpdatedAt.IsZero() || a.UpdatedAt.Before(a.CreatedAt) {
		return fmt.Errorf("invalid asset identity, namespace or timestamps")
	}
	if a.Path != "" {
		if err := ValidateRepositoryPath(a.Path); err != nil {
			return err
		}
	}
	return a.Object.Validate()
}

// ValidateRepositoryPath accepts only normalized relative POSIX paths outside
// Git internals and TOUKO metadata. It applies to both files and directories.
func ValidateRepositoryPath(value string) error {
	if value == "" || value == "." || path.Clean(value) != value || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return fmt.Errorf("invalid repository path %q", value)
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." || strings.EqualFold(part, ".git") || strings.EqualFold(part, ".touko") {
			return fmt.Errorf("reserved or unsafe repository path %q", value)
		}
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("control character in repository path %q", value)
		}
	}
	return nil
}
