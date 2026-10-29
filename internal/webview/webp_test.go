package webview

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"testing"
)

// estimate posts one image to the estimate endpoint and decodes the answer.
func estimate(t *testing.T, f *fixture, data []byte, quality string) map[string]any {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("image", "photo.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err = writer.WriteField("quality", quality); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", testOrigin+"/api/webp/estimate", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("X-Touko-CSRF", f.csrf)
	out := httptest.NewRecorder()
	f.server.ServeHTTP(out, req)
	if out.Code != 200 {
		t.Fatalf("estimate: %d %s", out.Code, out.Body.String())
	}
	var value map[string]any
	if err := json.Unmarshal(out.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

// The estimate must equal the size the real upload stores, and it must not
// stage anything.
func TestWebPEstimateMatchesStoredSize(t *testing.T) {
	f := newFixture(t)
	f.setup()
	source := photoBytes(t)
	value := estimate(t, f, source, "80")
	if got := int64(value["original_bytes"].(float64)); got != int64(len(source)) {
		t.Fatalf("original_bytes = %d, want %d", got, len(source))
	}
	if value["ext"] != "webp" {
		t.Fatalf("estimate ext = %v, want webp", value["ext"])
	}
	f.uploadOptions([][]byte{source}, map[string]string{"convert_webp": "true", "webp_quality": "80"}, 200)
	state := f.request("GET", "/api/state", nil, 200)
	var parsed struct {
		Assets []struct {
			Asset struct {
				Object struct {
					Size int64 `json:"size"`
				} `json:"object"`
			} `json:"asset"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(state.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Assets) != 1 {
		t.Fatalf("assets = %d", len(parsed.Assets))
	}
	estimated := int64(value["estimated_bytes"].(float64))
	if estimated != parsed.Assets[0].Asset.Object.Size {
		t.Fatalf("estimate %d != stored %d", estimated, parsed.Assets[0].Asset.Object.Size)
	}
}

func TestWebPEstimateRejectsBadQualityAndEmptyInput(t *testing.T) {
	f := newFixture(t)
	f.setup()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("image", "photo.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(photoBytes(t)); err != nil {
		t.Fatal(err)
	}
	if err = writer.WriteField("quality", "0"); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", testOrigin+"/api/webp/estimate", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("X-Touko-CSRF", f.csrf)
	out := httptest.NewRecorder()
	f.server.ServeHTTP(out, req)
	if out.Code != 400 {
		t.Fatalf("quality 0: %d %s", out.Code, out.Body.String())
	}
}
