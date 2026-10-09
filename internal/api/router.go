package api

import "net/http"

// NewRouter creates a new HTTP handler for the Waystone controller.
func NewRouter(version string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz(version))
	return mux
}
