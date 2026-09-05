// Package app assembles the adapter services. The skeleton stage only wires
// the health endpoint; real routing, storage, and WPS clients land with the
// later migration tasks and must not change the /healthz contract.
package app

import (
	"encoding/json"
	"net/http"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/config"
)

// Application owns the process-wide services.
type Application struct {
	Config  config.Config
	Version string
}

// healthPayload mirrors the Python /healthz body byte for byte.
type healthPayload struct {
	Status       string `json:"status"`
	Service      string `json:"service"`
	Version      string `json:"version"`
	NetworkCalls string `json:"network_calls"`
}

// Handler returns the skeleton HTTP handler.
func (a *Application) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, err := json.Marshal(healthPayload{
			Status:       "ok",
			Service:      "wps-enterprise-adapter",
			Version:      a.Version,
			NetworkCalls: "on-demand",
		})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		NotFound(w)
	})
	return mux
}

// NotFound mirrors the plain-text fallback for unknown skeleton routes.
func NotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("unknown route\n"))
}
