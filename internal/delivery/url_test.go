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
	for _, bad := range []string{"../secret", "objects/../sha256/aa/aa/" + h + ".png", "objects/blog/sha256/bb/aa/" + h + ".png", p + "?x"} {
		if _, err = BuildURL(l, bad, GitHub); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	if _, err = BuildURL(l, p, "unknown"); err == nil {
		t.Fatal("unknown provider accepted")
	}
}
