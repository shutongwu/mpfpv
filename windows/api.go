//go:build windows

package main

import (
	"embed"
	"encoding/json"
	"io"
	"net/http"

	log "github.com/sirupsen/logrus"
)

//go:embed static
var staticFS embed.FS

// NewAPIMux creates the HTTP handler for the simplified GUI.
func NewAPIMux(ctrl *Controller) http.Handler {
	mux := http.NewServeMux()

	// Serve embedded HTML.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		data, err := staticFS.ReadFile("static/index.html")
		if err != nil {
			http.Error(w, "internal error", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})

	// Status API.
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, ctrl.GetStatus())
	})

	// Config API.
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, ctrl.GetConfig())
	})

	// List all network interfaces.
	mux.HandleFunc("/api/interfaces", func(w http.ResponseWriter, r *http.Request) {
		nics, err := ListAllNICs()
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, nics)
	})

	// Connect API.
	mux.HandleFunc("/api/connect", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req ConnectRequest
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		if err := ctrl.Connect(req); err != nil {
			log.WithError(err).Error("gui: connect failed")
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
	})

	// Disconnect API.
	mux.HandleFunc("/api/disconnect", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		if err := ctrl.Disconnect(); err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
	})

	return mux
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
