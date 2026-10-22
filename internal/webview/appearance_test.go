package webview

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAppearancePersistsAcrossOriginsAndRejectsInvalidValues(t *testing.T) {
	f := newFixture(t)
	wanted := appearanceSettings{UIScale: 125, FontScale: 140}
	f.request(http.MethodPost, "/api/appearance", wanted, http.StatusOK)
	for _, body := range []any{
		appearanceSettings{UIScale: 74, FontScale: 140},
		appearanceSettings{UIScale: 125, FontScale: 201},
		appearanceSettings{UIScale: 126, FontScale: 140},
		map[string]int{"font_scale": 150},
	} {
		f.request(http.MethodPost, "/api/appearance", body, http.StatusBadRequest)
	}

	const nextOrigin = "http://127.0.0.1:45874"
	restarted, err := New(f.service, "", nextOrigin)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, nextOrigin+"/api/appearance", nil)
	response := httptest.NewRecorder()
	restarted.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("load after origin change: %d %s", response.Code, response.Body.String())
	}
	var got appearanceSettings
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got != wanted {
		t.Fatalf("saved display settings changed after invalid writes/restart: got %+v, want %+v", got, wanted)
	}
}
