package web

import (
	"encoding/json"
	"io"
	"net/http"
)

// accountBackup handles exporting all accounts to a password-encrypted backup file.
func (s *Server) accountBackup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if !s.validAdminSession(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "administrator login required")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body); err != nil {
		writeOpenAIError(w, 400, "invalid_request_error", "bad json")
		return
	}
	if len(body.Password) < 6 {
		writeOpenAIError(w, 400, "invalid_request_error", "password must be at least 6 characters")
		return
	}
	data, err := s.tokens.ExportAccounts(body.Password)
	if err != nil {
		writeOpenAIError(w, 500, "backup_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="m365-accounts-backup.json"`)
	_, _ = w.Write(data)
}

// accountRestore handles importing accounts from a password-encrypted backup file.
func (s *Server) accountRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if !s.validAdminSession(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "administrator login required")
		return
	}
	// Parse multipart form: "file" = backup JSON, "password" = decryption password
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		// Fallback: try JSON body with embedded data
		var body struct {
			Password string          `json:"password"`
			Data     json.RawMessage `json:"data"`
		}
		if jsonErr := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&body); jsonErr != nil {
			writeOpenAIError(w, 400, "invalid_request_error", "expected multipart file upload or JSON with data field")
			return
		}
		if len(body.Password) < 6 {
			writeOpenAIError(w, 400, "invalid_request_error", "password must be at least 6 characters")
			return
		}
		if len(body.Data) == 0 {
			writeOpenAIError(w, 400, "invalid_request_error", "backup data is required")
			return
		}
		imported, err := s.tokens.ImportAccounts(body.Data, body.Password)
		if err != nil {
			writeOpenAIError(w, 400, "restore_error", err.Error())
			return
		}
		jsonOut(w, map[string]any{"ok": true, "imported": imported})
		return
	}

	password := r.FormValue("password")
	if len(password) < 6 {
		writeOpenAIError(w, 400, "invalid_request_error", "password must be at least 6 characters")
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeOpenAIError(w, 400, "invalid_request_error", "backup file is required")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 16<<20))
	if err != nil {
		writeOpenAIError(w, 400, "invalid_request_error", "failed to read backup file")
		return
	}
	imported, err := s.tokens.ImportAccounts(data, password)
	if err != nil {
		writeOpenAIError(w, 400, "restore_error", err.Error())
		return
	}
	jsonOut(w, map[string]any{"ok": true, "imported": imported})
}
