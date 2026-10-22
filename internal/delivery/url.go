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
	JSDelivr   Provider = "jsdelivr"
	GitHub     Provider = "github"
	Cloudflare Provider = "cloudflare"
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

type cloudflareBase struct {
	host   string
	prefix []string
}

// parseCloudflareBaseURL is the single validator behind both the exported
// validator and URL builder. It accepts an absolute HTTPS URL whose optional
// path is a safe repository-path prefix.
func parseCloudflareBaseURL(raw string) (cloudflareBase, error) {
	invalid := fmt.Errorf("invalid Cloudflare public base URL")
	if strings.ContainsRune(raw, '#') {
		return cloudflareBase{}, invalid
	}
	u, err := url.Parse(raw)
	if err != nil {
		return cloudflareBase{}, invalid
	}
	if !strings.EqualFold(u.Scheme, "https") || u.Opaque != "" || u.Host == "" || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" ||
		strings.ContainsAny(u.Host, " \t\r\n\\") {
		return cloudflareBase{}, invalid
	}
	base := cloudflareBase{host: u.Host}
	p := u.Path
	if p == "" || p == "/" {
		return base, nil
	}
	if !strings.HasPrefix(p, "/") {
		return cloudflareBase{}, invalid
	}
	p = strings.TrimPrefix(p, "/")
	if strings.HasSuffix(p, "/") {
		p = strings.TrimSuffix(p, "/")
	}
	if p == "" || strings.HasSuffix(p, "/") {
		return cloudflareBase{}, invalid
	}
	if err := domain.ValidateRepositoryPath(p); err != nil {
		return cloudflareBase{}, invalid
	}
	base.prefix = strings.Split(p, "/")
	return base, nil
}

// ValidateCloudflareBaseURL checks a public mirror base URL without building a
// delivery URL.
func ValidateCloudflareBaseURL(raw string) error {
	_, err := parseCloudflareBaseURL(raw)
	return err
}

// BuildCloudflareURL joins a configured public base URL and an object path.
// The object path is always escaped segment by segment, so a literal '%' is
// never mistaken for an already-escaped byte.
func BuildCloudflareURL(publicBaseURL, objectPath string) (string, error) {
	base, err := parseCloudflareBaseURL(publicBaseURL)
	if err != nil {
		return "", err
	}
	if err = domain.ValidateRepositoryPath(objectPath); err != nil {
		return "", err
	}
	segments := make([]string, 0, len(base.prefix)+1)
	for _, part := range base.prefix {
		segments = append(segments, url.PathEscape(part))
	}
	for _, part := range strings.Split(objectPath, "/") {
		segments = append(segments, url.PathEscape(part))
	}
	return "https://" + base.host + "/" + strings.Join(segments, "/"), nil
}
