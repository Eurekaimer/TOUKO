package webview

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestHashCacheArchivesDuplicatesAfterOptIn(t *testing.T) {
	f := newFixture(t)
	f.setup()
	picture, other := imageBytes(t), photoBytes(t)
	f.upload([][]byte{picture, picture, other}, http.StatusOK)
	f.request("POST", "/api/commit", map[string]string{"message": "publish hashless batch"}, http.StatusOK)
	readReport := func() hashReport {
		t.Helper()
		var report hashReport
		body := f.request("GET", "/api/hash-cache", nil, http.StatusOK).Body.Bytes()
		if err := json.Unmarshal(body, &report); err != nil {
			t.Fatal(err)
		}
		return report
	}
	if report := readReport(); report.Enabled || len(report.Assets) != 0 {
		t.Fatalf("hash cache is not off by default: %+v", report)
	}
	f.request("POST", "/api/hash-cache", map[string]bool{"enabled": true}, http.StatusOK)
	deadline := time.Now().Add(5 * time.Second)
	for report := readReport(); report.Running; report = readReport() {
		if time.Now().After(deadline) {
			t.Fatalf("hash scan did not complete: %+v", report)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Nothing is carried across to the replacement server except workspace files.
	server, err := New(f.service, "sentinel-private-token", testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	f.server = server
	report := readReport()
	if !report.Enabled || report.Total != 3 || len(report.Assets) != 3 || report.Error != "" {
		t.Fatalf("archive not persisted: %+v", report)
	}
	var duplicates, unique int
	for _, row := range report.Assets {
		switch row.Hash {
		case sha256.Sum256(picture):
			duplicates++
		case sha256.Sum256(other):
			unique++
		}
	}
	if duplicates != 2 || unique != 1 {
		t.Fatalf("wrong duplicate hashes: %+v", report.Assets)
	}
}
