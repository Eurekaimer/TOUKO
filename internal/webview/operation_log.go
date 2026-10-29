package webview

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// recordOperationFailure stores only a redacted error, never request bodies or credentials.
func (s *Server) recordOperationFailure(operation string, status int, code, message string) error {
	message = s.redactMessage(message)
	if chars := []rune(message); len(chars) > 2048 {
		message = string(chars[:2048])
	}
	record, err := json.Marshal(struct {
		Time      string `json:"time"`
		Workspace string `json:"workspace"`
		Operation string `json:"operation"`
		Status    int    `json:"http_status"`
		Code      string `json:"code"`
		Message   string `json:"message"`
	}{time.Now().UTC().Format(time.RFC3339), s.workspaceID, operation, status, code, message})
	if err != nil {
		return err
	}
	owner := s.authOwner()
	owner.logMu.Lock()
	defer owner.logMu.Unlock()
	file, err := os.OpenFile(filepath.Join(owner.service.Dir, "operation-errors.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		return err
	}
	_, writeErr := file.Write(append(record, '\n'))
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func loggedOperation(path string) bool {
	switch strings.TrimPrefix(path, "/api/") {
	case "upload", "commit", "sync", "cloudflare/sync", "reindex", "verify", "import", "reset":
		return true
	}
	return false
}
