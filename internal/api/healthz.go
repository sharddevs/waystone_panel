package api

import (
	"encoding/json"
	"log"
	"net/http"
)

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

		if err := json.NewEncoder(w).Encode(response); err != nil {
			log.Printf("failed to encode health response: %v", err)
		}

	}
}
