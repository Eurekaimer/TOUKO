package delivery

import (
	"errors"
	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/repository"
	"net/url"
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

func TestCloudflareURLRoutesAndEscaping(t *testing.T) {
	for _, tc := range []struct {
		name, base, object, want string
	}{
		{"plain", "https://img.eurekaimer.icu", "images/yuri/touko.webp", "https://img.eurekaimer.icu/images/yuri/touko.webp"},
		{"trailing-slash", "https://img.eurekaimer.icu/", "images/yuri/touko.webp", "https://img.eurekaimer.icu/images/yuri/touko.webp"},
		{"prefix", "https://cdn.example.com/media/archive/", "objects/blog/a b.png", "https://cdn.example.com/media/archive/objects/blog/a%20b.png"},
		{"escaped-prefix", "https://cdn.example.com/%E5%9B%BE%20%E9%9B%86/", "旧图.webp", "https://cdn.example.com/%E5%9B%BE%20%E9%9B%86/%E6%97%A7%E5%9B%BE.webp"},
		{"unicode-specials", "https://cdn.example.com", "images/灯子+(1)%20.webp", "https://cdn.example.com/images/%E7%81%AF%E5%AD%90+%281%29%2520.webp"},
		{"hash-query-percent", "https://cdn.example.com", "uploads/a #?%.png", "https://cdn.example.com/uploads/a%20%23%3F%25.png"},
		{"no-suffix", "https://cdn.example.com", "img/旧图 无后缀", "https://cdn.example.com/img/%E6%97%A7%E5%9B%BE%20%E6%97%A0%E5%90%8E%E7%BC%80"},
		{"percent-object-literal", "https://cdn.example.com", "a%20b.webp", "https://cdn.example.com/a%2520b.webp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BuildCloudflareURL(tc.base, tc.object)
			if err != nil || got != tc.want {
				t.Fatalf("BuildCloudflareURL(%q, %q) = %q, %v; want %q", tc.base, tc.object, got, err, tc.want)
			}
			u, err := url.Parse(got)
			if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
				t.Fatalf("unsafe built URL %q: %v", got, err)
			}
		})
	}
}

func TestCloudflareBaseValidation(t *testing.T) {
	for _, raw := range []string{
		"https://img.eurekaimer.icu",
		"https://img.eurekaimer.icu/",
		"https://cdn.example.com/media",
		"https://cdn.example.com/media/",
		"https://cdn.example.com/my%20pics/%E5%9B%BE/",
		"https://cdn.example.com:8443/images/",
	} {
		if err := ValidateCloudflareBaseURL(raw); err != nil {
			t.Fatalf("ValidateCloudflareBaseURL(%q) = %v", raw, err)
		}
	}
	for _, raw := range []string{
		"",
		"img.example.com",
		"http://img.example.com",
		"ftp://img.example.com",
		"https://",
		"https://img.example.com?x=1",
		"https://img.example.com/?x=1",
		"https://img.example.com/#frag",
		"https://user:secret@img.example.com",
		"https://img.example.com/../secret",
		"https://img.example.com/a/%2e%2e/secret",
		"https://img.example.com/a/%2E%2E/secret",
		"https://img.example.com/a/./b",
		"https://img.example.com/a//b",
		"https://img.example.com/a%5Cb",
		"https://img.example.com/a%00b",
		"https://img.example.com/a%0Ab",
		"https://img.example.com/%zz",
		"https://img.example.com//",
		"https://img.example.com/images/%2Fetc/passwd",
		"https:opaque",
	} {
		if err := ValidateCloudflareBaseURL(raw); err == nil {
			t.Fatalf("ValidateCloudflareBaseURL accepted %q", raw)
		}
		if _, err := BuildCloudflareURL(raw, "images/touko.webp"); err == nil {
			t.Fatalf("BuildCloudflareURL accepted base %q", raw)
		}
	}
}

func TestCloudflareURLRejectsUnsafeObjectPaths(t *testing.T) {
	for _, bad := range []string{
		"",
		".",
		"../secret",
		"/img/photo.png",
		"img//photo.png",
		"img\\photo.png",
		"img/photo\n.png",
		"img/.git/photo.png",
		".touko/photo.png",
		"img/../photo.png",
	} {
		if _, err := BuildCloudflareURL("https://img.example.com", bad); err == nil {
			t.Fatalf("BuildCloudflareURL accepted object path %q", bad)
		}
	}
	location := repository.Location{Backend: "github", Owner: "owner", Name: "gallery", Branch: "main"}
	if _, err := BuildURL(location, "images/touko.webp", Cloudflare); err == nil {
		t.Fatal("BuildURL produced a Cloudflare URL without a configured mirror base")
	}
}
