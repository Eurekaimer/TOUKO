package credential_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/credential"
)

func setR2Environment(t *testing.T, accessKeyID, secretAccessKey string) {
	t.Helper()
	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", accessKeyID)
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", secretAccessKey)
}

func TestResolveR2ReadsOnlyExplicitEnvironment(t *testing.T) {
	ctx := context.Background()
	setR2Environment(t, "access-key-SENTINEL", "secret-key-SENTINEL")
	accessKeyID, secretAccessKey, err := credential.ResolveR2(ctx)
	if err != nil || accessKeyID != "access-key-SENTINEL" || secretAccessKey != "secret-key-SENTINEL" {
		t.Fatalf("explicit R2 credentials not returned: %q %q %v", accessKeyID, secretAccessKey, err)
	}

	setR2Environment(t, "", "secret-key-SENTINEL")
	accessKeyID, secretAccessKey, err = credential.ResolveR2(ctx)
	if err == nil || accessKeyID != "" || secretAccessKey != "" {
		t.Fatalf("missing access key id accepted: %q %q %v", accessKeyID, secretAccessKey, err)
	}
	if !strings.Contains(err.Error(), "TOUKO_R2_ACCESS_KEY_ID") || !strings.Contains(err.Error(), "TOUKO_R2_SECRET_ACCESS_KEY") {
		t.Fatalf("missing credential error does not name its variables: %v", err)
	}
	if strings.Contains(err.Error(), "SENTINEL") {
		t.Fatal("missing credential error disclosed a credential value")
	}

	setR2Environment(t, "access-key-SENTINEL", "")
	accessKeyID, secretAccessKey, err = credential.ResolveR2(ctx)
	if err == nil || accessKeyID != "" || secretAccessKey != "" {
		t.Fatalf("missing secret access key accepted: %q %q %v", accessKeyID, secretAccessKey, err)
	}
	if !strings.Contains(err.Error(), "TOUKO_R2_ACCESS_KEY_ID") || !strings.Contains(err.Error(), "TOUKO_R2_SECRET_ACCESS_KEY") {
		t.Fatalf("missing credential error does not name its variables: %v", err)
	}
	if strings.Contains(err.Error(), "SENTINEL") {
		t.Fatal("missing credential error disclosed a credential value")
	}
}

func TestResolveR2RejectsInvalidValuesWithoutDisclosure(t *testing.T) {
	for _, tc := range []struct {
		name, access, secret, variable string
	}{
		{"blank access", " ", "secret-SENTINEL", "TOUKO_R2_ACCESS_KEY_ID"},
		{"tab access", "access\tSENTINEL", "secret-SENTINEL", "TOUKO_R2_ACCESS_KEY_ID"},
		{"newline access", "access\nSENTINEL", "secret-SENTINEL", "TOUKO_R2_ACCESS_KEY_ID"},
		{"blank secret", "access-SENTINEL", " ", "TOUKO_R2_SECRET_ACCESS_KEY"},
		{"space secret", "access-SENTINEL", "secret SENTINEL", "TOUKO_R2_SECRET_ACCESS_KEY"},
		{"carriage return secret", "access-SENTINEL", "secret\rSENTINEL", "TOUKO_R2_SECRET_ACCESS_KEY"},
		{"non-breaking space secret", "access-SENTINEL", "secret\u00a0SENTINEL", "TOUKO_R2_SECRET_ACCESS_KEY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setR2Environment(t, tc.access, tc.secret)
			accessKeyID, secretAccessKey, err := credential.ResolveR2(context.Background())
			if err == nil || accessKeyID != "" || secretAccessKey != "" {
				t.Fatalf("invalid R2 credential accepted: %q %q %v", accessKeyID, secretAccessKey, err)
			}
			if !strings.Contains(err.Error(), tc.variable) {
				t.Fatalf("error does not name %s: %v", tc.variable, err)
			}
			if strings.Contains(err.Error(), "SENTINEL") {
				t.Fatal("invalid credential error disclosed a credential value")
			}
		})
	}
}

func TestResolveR2DoesNotFallBackToOtherCredentials(t *testing.T) {
	setR2Environment(t, "", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "aws-access-SENTINEL")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret-SENTINEL")
	t.Setenv("GITHUB_TOKEN", "github-SENTINEL")
	t.Setenv("GH_TOKEN", "gh-SENTINEL")
	t.Setenv("PATH", t.TempDir())
	accessKeyID, secretAccessKey, err := credential.ResolveR2(context.Background())
	if err == nil || accessKeyID != "" || secretAccessKey != "" {
		t.Fatalf("R2 credentials fell back to another source: %q %q %v", accessKeyID, secretAccessKey, err)
	}
	if strings.Contains(err.Error(), "SENTINEL") {
		t.Fatal("fallback error disclosed a credential value")
	}
}

func TestResolveR2RespectsCancellation(t *testing.T) {
	setR2Environment(t, "access-SENTINEL", "secret-SENTINEL")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	accessKeyID, secretAccessKey, err := credential.ResolveR2(ctx)
	if !errors.Is(err, context.Canceled) || accessKeyID != "" || secretAccessKey != "" {
		t.Fatalf("cancellation not preserved: %q %q %v", accessKeyID, secretAccessKey, err)
	}
}
