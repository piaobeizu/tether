package workspace

import (
	"encoding/json"
	"net/http"
	"strings"
)

// RegisterAPI wires workspace REST endpoints into mux (s7).
//
//	GET    /api/v1/workspaces          → list all workspaces
//	POST   /api/v1/workspaces          → add workspace {"name":"...","path":"..."}
//	DELETE /api/v1/workspaces/{id}     → remove workspace by ID
func RegisterAPI(mux *http.ServeMux, reg *Registry) {
	mux.HandleFunc("/api/v1/workspaces", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			jsonResp(w, reg.List())
		case http.MethodPost:
			var body struct {
				Name string `json:"name"`
				Path string `json:"path"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			ws, err := reg.Add(body.Name, body.Path)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			jsonResp(w, ws)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/v1/workspaces/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/workspaces/")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := reg.Remove(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func jsonResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
