package api

import (
	"encoding/json"
	"net/http"
)

type errorResponse struct {
	Error string `json:"error"`
}

// NewRouter returns the HTTP handler with all API routes registered.
func NewRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /payments", createPayments)
	return mux
}

// createPayments is a stub that always responds with 422 Unprocessable Entity.
func createPayments(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusUnprocessableEntity, errorResponse{Error: "not implemented"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
