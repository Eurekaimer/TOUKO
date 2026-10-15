package domain

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"
)

func TestIdentityAndSelectors(t *testing.T) {
	a, err := NewAssetID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewAssetID()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("independent adds must have independent identities")
	}
	h := ObjectHash(sha256.Sum256([]byte("same bytes")))
	data, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var got ObjectHash
	if err = json.Unmarshal(data, &got); err != nil || got != h {
		t.Fatalf("hash round trip: %v", err)
	}
	for _, bad := range []string{`"ABCDEF"`, `"` + "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF" + `"`, `"../hash"`} {
		if json.Unmarshal([]byte(bad), &got) == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	assets := map[AssetID]Asset{a: {ID: a}, b: {ID: b}}
	for _, s := range []string{a.String(), a.String()[:8]} {
		id, err := ResolveAsset(assets, s)
		if err != nil || id != a {
			t.Fatalf("resolve %s: %v", s, err)
		}
	}
	if _, err := ResolveAsset(assets, a.String()[:7]); !errors.Is(err, ErrAssetNotFound) {
		t.Fatal(err)
	}
	c, _ := ParseAssetID("12345678-0000-4000-8000-000000000001")
	d, _ := ParseAssetID("12345678-0000-4000-8000-000000000002")
	if _, err := ResolveAsset(map[AssetID]Asset{c: {ID: c}, d: {ID: d}}, "12345678"); !errors.Is(err, ErrAmbiguousSelector) {
		t.Fatal(err)
	}
}

func TestNamespace(t *testing.T) {
	for _, s := range []string{"default", "blog", "a_b-1", ""} {
		if _, err := ParseNamespace(s); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []string{"..", "../blog", "Blog", "图片", "a/b", "a.b"} {
		if _, err := ParseNamespace(s); err == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}
