package credential

import (
	"context"
	"fmt"
	"os"
	"unicode"
)

// ResolveR2 reads only the explicit Cloudflare R2 credential variables.
// Errors name variables but never include their values.
func ResolveR2(ctx context.Context) (accessKeyID, secretAccessKey string, err error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	accessKeyID = os.Getenv("TOUKO_R2_ACCESS_KEY_ID")
	secretAccessKey = os.Getenv("TOUKO_R2_SECRET_ACCESS_KEY")
	if accessKeyID == "" || secretAccessKey == "" {
		return "", "", fmt.Errorf("Cloudflare R2 credentials require TOUKO_R2_ACCESS_KEY_ID and TOUKO_R2_SECRET_ACCESS_KEY")
	}
	if !validR2Credential(accessKeyID) {
		return "", "", fmt.Errorf("invalid TOUKO_R2_ACCESS_KEY_ID credential")
	}
	if !validR2Credential(secretAccessKey) {
		return "", "", fmt.Errorf("invalid TOUKO_R2_SECRET_ACCESS_KEY credential")
	}
	return accessKeyID, secretAccessKey, nil
}

func validR2Credential(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}
