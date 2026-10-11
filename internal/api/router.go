// Package api is the controller's HTTP API. So far it only has /healthz.
package api

import "net/http"

// NewRouter returns the controller's HTTP handler. version is what
// /healthz reports.
func NewRouter(version string) http.Handler {
	mux := http.NewServeMux()
	// The "GET " in the pattern (Go 1.22+) makes the mux answer other
	// methods with 405 for us.
	mux.HandleFunc("GET /healthz", healthz(version))
	return mux
}
