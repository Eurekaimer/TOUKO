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
	if err := domain.ValidateRepositoryPath(objectPath); err != nil {
		return "", err
	}
	parts := strings.Split(objectPath, "/")
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
