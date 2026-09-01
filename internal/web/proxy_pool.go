package web

import (
	"encoding/json"
	"m365-copilot2api/internal/outbound"
	"net/http"
	"strings"
)

func (s *Server) persistProxyPool() error {
	// No-op: sing-box subscription is env-driven, not persisted in settings.
	return nil
}

func (s *Server) proxyPool(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jsonOut(w, map[string]any{"proxies": outbound.ProxyPoolStatus()})
	case http.MethodPost:
		// Reconfigure sing-box with a new subscription URL
		var body struct {
			Subscription string `json:"subscription"`
			URL          string  `json:"url"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body) != nil {
			writeOpenAIError(w, 400, "invalid_request_error", "bad json")
			return
		}
		subURL := strings.TrimSpace(body.Subscription)
		if subURL == "" {
			subURL = strings.TrimSpace(body.URL)
		}
		if subURL == "" {
			writeOpenAIError(w, 400, "invalid_request_error", "subscription url required")
			return
		}
		if err := outbound.ConfigureSingBox(subURL); err != nil {
			writeOpenAIError(w, 400, "invalid_request_error", err.Error())
			return
		}
		jsonOut(w, map[string]any{"ok": true, "proxies": outbound.ProxyPoolStatus()})
	case http.MethodDelete:
		// Stop sing-box
		outbound.StopSingBox()
		jsonOut(w, map[string]any{"ok": true, "proxies": []map[string]any{}})
	default:
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}
