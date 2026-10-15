package delivery

import (
	"fmt"
	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/repository"
	"net/url"
	"strings"
)

type Provider string

const (
	JSDelivr Provider = "jsdelivr"
	GitHub   Provider = "github"
)

func BuildURL(location repository.Location, objectPath string, provider Provider) (string, error) {
	if location.Backend != "github" || location.Private {
		return "", fmt.Errorf("%w: URLs require a public GitHub repository", domain.ErrDeliveryUnavailable)
	}
	if err := location.Validate(); err != nil {
		return "", err
	}
	parts := strings.Split(objectPath, "/")
	if len(parts) != 6 || parts[0] != "objects" || parts[2] != "sha256" {
		return "", fmt.Errorf("invalid object path")
	}
	ns, err := domain.ParseNamespace(parts[1])
	if err != nil || string(ns) != parts[1] {
		return "", fmt.Errorf("invalid object namespace")
	}
	filename := strings.Split(parts[5], ".")
	if len(filename) != 2 {
		return "", fmt.Errorf("invalid object filename")
	}
	var hash domain.ObjectHash
	if err = hash.UnmarshalText([]byte(filename[0])); err != nil {
		return "", err
	}
	if (filename[1] != "png" && filename[1] != "jpg" && filename[1] != "gif") || parts[3] != filename[0][:2] || parts[4] != filename[0][2:4] {
		return "", fmt.Errorf("invalid canonical object path")
	}
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	path := strings.Join(parts, "/")
	owner, name, branch := url.PathEscape(location.Owner), url.PathEscape(location.Name), url.PathEscape(location.Branch)
	switch provider {
	case JSDelivr:
		return "https://cdn.jsdelivr.net/gh/" + owner + "/" + name + "@" + branch + "/" + path, nil
	case GitHub:
		return "https://raw.githubusercontent.com/" + owner + "/" + name + "/refs/heads/" + branch + "/" + path, nil
	default:
		return "", fmt.Errorf("unknown delivery provider %q", provider)
	}
}
