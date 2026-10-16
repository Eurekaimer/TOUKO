package credential_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/credential"
)

func fakeGH(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
}

func TestResolvePrecedenceAndInvalidExplicitCredential(t *testing.T) {
	fakeGH(t, "printf 'keyring-secret\\n'")
	t.Setenv("GH_TOKEN", "secondary-secret")
	t.Setenv("GITHUB_TOKEN", "primary-secret")
	token, source, err := credential.Resolve(context.Background())
	if err != nil || token != "primary-secret" || source != "github_token" {
		t.Fatalf("explicit primary credential not selected (source=%s, err=%v)", source, err)
	}
	t.Setenv("GITHUB_TOKEN", "invalid secret")
	token, source, err = credential.Resolve(context.Background())
	if err == nil || token != "" || source != "github_token" || strings.Contains(err.Error(), "invalid secret") {
		t.Fatal("invalid explicit credential fell back or was disclosed")
	}
	t.Setenv("GITHUB_TOKEN", "")
	token, source, err = credential.Resolve(context.Background())
	if err != nil || token != "secondary-secret" || source != "gh_token" {
		t.Fatal("secondary explicit credential not selected")
	}
	t.Setenv("GH_TOKEN", "")
	token, source, err = credential.Resolve(context.Background())
	if err != nil || token != "keyring-secret" || source != "gh" {
		t.Fatal("keyring credential not selected")
	}
}

func TestResolveAnonymousAndSafeExecutionFailure(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantError  bool
	}{
		{"logged-out", "printf 'no oauth token found for github.com\\n' >&2; exit 1", false},
		{"unexpected-failure", "printf 'stderr-secret' >&2; exit 2", true},
		{"unexpected-exit-one", "printf 'stderr-secret' >&2; exit 1", true},
		{"invalid-output", "printf 'first-secret\\nsecond-secret\\n'", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeGH(t, tc.body)
			token, source, err := credential.Resolve(context.Background())
			if (err != nil) != tc.wantError || token != "" {
				t.Fatalf("unexpected resolution: source=%s err=%v", source, err)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("credential process output disclosed")
			}
			if err == nil && source != "anonymous" {
				t.Fatalf("logged-out source=%s", source)
			}
		})
	}
	t.Run("missing-gh", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "")
		t.Setenv("GH_TOKEN", "")
		t.Setenv("PATH", t.TempDir())
		token, source, err := credential.Resolve(context.Background())
		if err != nil || token != "" || source != "anonymous" {
			t.Fatalf("missing gh blocked anonymous access: %s %v", source, err)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		fakeGH(t, "printf 'secret'")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, err := credential.Resolve(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation hidden: %v", err)
		}
	})
}
