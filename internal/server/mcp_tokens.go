package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/piaobeizu/tether/internal/auth/apitoken"
)

// RegisterMCPTokensAPI wires POST/GET/DELETE CRUD endpoints onto mux.
// Pass nil logger to use slog.Default().
func RegisterMCPTokensAPI(mux *http.ServeMux, store *apitoken.Store, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	mux.HandleFunc("POST /api/v1/mcp/tokens", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		raw, tok, err := store.Create(body.Name)
		if err != nil {
			switch {
			case errors.Is(err, apitoken.ErrNameRequired) || errors.Is(err, apitoken.ErrNameTooLong):
				http.Error(w, "name invalid", http.StatusBadRequest)
			case errors.Is(err, apitoken.ErrTooManyTokens):
				http.Error(w, "token store full", http.StatusConflict)
			default:
				slog.Error("mcp.apitoken.create failed", "err", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
			return
		}
		logger.Info("mcp.apitoken.created", "id", tok.ID, "name", tok.Name)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			ID        string    `json:"id"`
			Name      string    `json:"name"`
			Token     string    `json:"token"`
			CreatedAt time.Time `json:"created_at"`
		}{tok.ID, tok.Name, raw, tok.CreatedAt})
	})

	mux.HandleFunc("GET /api/v1/mcp/tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Tokens []apitoken.View `json:"tokens"`
		}{store.List()})
	})

	mux.HandleFunc("DELETE /api/v1/mcp/tokens/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := store.Revoke(id); err != nil {
			if errors.Is(err, apitoken.ErrNotFound) {
				http.Error(w, "not found", http.StatusNotFound)
			} else {
				slog.Error("mcp.apitoken.revoke failed", "id", id, "err", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
			return
		}
		logger.Info("mcp.apitoken.revoked", "id", id)
		w.WriteHeader(http.StatusNoContent)
	})
}
