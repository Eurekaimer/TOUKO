package repository

import (
	"fmt"
	"path/filepath"
	"strings"
)

type Location struct {
	Backend string `json:"backend"`
	Branch  string `json:"branch"`
	Path    string `json:"path,omitempty"`
	Owner   string `json:"owner,omitempty"`
	Name    string `json:"name,omitempty"`
	Private bool   `json:"private"`
}

func (l Location) Validate() error {
	if l.Branch == "" || strings.HasPrefix(l.Branch, "-") || strings.ContainsAny(l.Branch, "\x00\r\n") {
		return fmt.Errorf("invalid branch")
	}
	switch l.Backend {
	case "localgit":
		if !filepath.IsAbs(l.Path) || l.Owner != "" || l.Name != "" {
			return fmt.Errorf("localgit requires an absolute path")
		}
	case "github":
		if l.Owner == "" || l.Name == "" || strings.ContainsAny(l.Owner+l.Name, "/\\\x00\r\n") || l.Path != "" {
			return fmt.Errorf("github requires owner and repository names")
		}
	default:
		return fmt.Errorf("unsupported backend %q", l.Backend)
	}
	return nil
}
