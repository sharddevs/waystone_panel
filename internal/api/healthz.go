package api

import (
	"encoding/json"
	"log"
	"net/http"
)

// healthz returns a handler that says the controller is up and which
// version it is running.
func healthz(version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		response := struct {
			Status  string `json:"status"`
			Version string `json:"version"`
		}{
			Status:  "ok",
			Version: version,
		}

		// If this fails the 200 has already gone out, so logging is all
		// we can do.
		if err := json.NewEncoder(w).Encode(response); err != nil {
			log.Printf("failed to encode health response: %v", err)
		}

	}
}
