package webview

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
)

type appearanceSettings struct {
	UIScale   int `json:"ui_scale"`
	FontScale int `json:"font_scale"`
}

func (a appearanceSettings) validate() error {
	if a.UIScale < 75 || a.UIScale > 200 || a.FontScale < 75 || a.FontScale > 200 || a.UIScale%5 != 0 || a.FontScale%5 != 0 {
		return usage("UI and font scales must be multiples of 5 between 75 and 200")
	}
	return nil
}

// Display preferences belong to this device, not to a repository or browser
// origin: the desktop listener can use a different port after every restart.
func (s *Server) appearanceAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		s.fail(w, http.StatusMethodNotAllowed, "usage", "method not allowed")
		return
	}
	settings := appearanceSettings{UIScale: 100, FontScale: 100}
	if r.Method == http.MethodPost {
		settings = appearanceSettings{}
		if err := decodeJSON(w, r, &settings); err != nil {
			s.respondError(w, err)
			return
		}
	}
	s.spaces.mu.Lock()
	defer s.spaces.mu.Unlock()
	path := filepath.Join(s.spaces.root.service.Dir, "appearance.json")
	var err error
	if r.Method == http.MethodGet {
		var data []byte
		data, err = os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		} else if err == nil {
			err = json.Unmarshal(data, &settings)
		}
	}
	if err == nil {
		err = settings.validate()
	}
	if err == nil && r.Method == http.MethodPost {
		err = saveAppearance(path, settings)
	}
	if err != nil {
		s.respondError(w, fmt.Errorf("display preferences: %w", err))
		return
	}
	s.json(w, struct {
		appearanceSettings
		CSRF string `json:"csrf"`
	}{settings, s.csrf})
}

func saveAppearance(path string, settings appearanceSettings) error {
	data, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), "appearance-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err := file.Write(append(data, '\n')); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
