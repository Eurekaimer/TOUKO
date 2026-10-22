package delivery

import (
	"errors"
	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/repository"
	"strings"
	"testing"
)

func TestDeliveryRoutesAndRestrictions(t *testing.T) {
	h := strings.Repeat("a", 64)
	p := "objects/blog/sha256/aa/aa/" + h + ".png"
	l := repository.Location{Backend: "github", Owner: "owner", Name: "gallery", Branch: "feature/demo"}
	got, err := BuildURL(l, p, JSDelivr)
	if err != nil || got != "https://cdn.jsdelivr.net/gh/owner/gallery@feature%2Fdemo/"+p {
		t.Fatal(got, err)
	}
	got, err = BuildURL(l, p, GitHub)
	if err != nil || got != "https://raw.githubusercontent.com/owner/gallery/refs/heads/feature%2Fdemo/"+p {
		t.Fatal(got, err)
	}
	l.Private = true
	if _, err = BuildURL(l, p, GitHub); !errors.Is(err, domain.ErrDeliveryUnavailable) {
		t.Fatal(err)
	}
	l.Private = false
	l.Backend = "localgit"
	if _, err = BuildURL(l, p, JSDelivr); !errors.Is(err, domain.ErrDeliveryUnavailable) {
		t.Fatal(err)
	}
	l.Backend = "github"
	for _, bad := range []string{"../secret", "objects/../sha256/aa/aa/" + h + ".png", "/img/photo.png", "img/.git/photo.png", ".touko/photo.png", "img\\photo.png", "img/photo\n.png", "img//photo.png"} {
		if _, err = BuildURL(l, bad, GitHub); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	if _, err = BuildURL(l, p, "unknown"); err == nil {
		t.Fatal("unknown provider accepted")
	}
}

func TestLegacyImageDeliveryRoutes(t *testing.T) {
	location := repository.Location{Backend: "github", Owner: "owner", Name: "gallery", Branch: "feature/demo"}
	for _, test := range []struct {
		path, escaped string
	}{
		{"img/旧图 无后缀", "img/%E6%97%A7%E5%9B%BE%20%E6%97%A0%E5%90%8E%E7%BC%80"},
		{"uploads/a #?%.png", "uploads/a%20%23%3F%25.png"},
		{"images/灯子+(1)%20.webp", "images/%E7%81%AF%E5%AD%90+%281%29%2520.webp"},
	} {
		for provider, prefix := range map[Provider]string{
			GitHub:   "https://raw.githubusercontent.com/owner/gallery/refs/heads/feature%2Fdemo/",
			JSDelivr: "https://cdn.jsdelivr.net/gh/owner/gallery@feature%2Fdemo/",
		} {
			got, err := BuildURL(location, test.path, provider)
			if err != nil || got != prefix+test.escaped {
				t.Fatalf("%s %q = %q, %v; want %q", provider, test.path, got, err, prefix+test.escaped)
			}
		}
	}
}
