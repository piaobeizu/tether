package skill

import (
	"encoding/json"
	"net/http"
	"strings"
)

// RegisterAPI wires skill REST endpoints into mux (s7).
//
//	GET  /api/v1/skills                           → list all skills
//	POST /api/v1/skills                           → install skill {"name":"...","sourcePath":"..."}
//	DELETE /api/v1/skills/{id}                    → remove skill
//	POST /api/v1/skills/{id}/enable               → enable in workspace {"workspacePath":"..."}
//	POST /api/v1/skills/{id}/disable              → disable in workspace {"workspacePath":"..."}
func RegisterAPI(mux *http.ServeMux, reg *Registry) {
	mux.HandleFunc("/api/v1/skills", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			jsonResp(w, reg.List())
		case http.MethodPost:
			var body struct {
				Name       string `json:"name"`
				SourcePath string `json:"sourcePath"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.SourcePath == "" {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			sk, err := reg.Install(body.Name, body.SourcePath)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			jsonResp(w, sk)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/v1/skills/", func(w http.ResponseWriter, r *http.Request) {
		tail := strings.TrimPrefix(r.URL.Path, "/api/v1/skills/")
		parts := strings.SplitN(tail, "/", 2)
		id := parts[0]
		action := ""
		if len(parts) == 2 {
			action = parts[1]
		}

		switch {
		case action == "" && r.Method == http.MethodDelete:
			if err := reg.Remove(id); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)

		case action == "enable" && r.Method == http.MethodPost:
			var body struct {
				WorkspacePath string `json:"workspacePath"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.WorkspacePath == "" {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if err := reg.Enable(id, body.WorkspacePath); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)

		case action == "disable" && r.Method == http.MethodPost:
			var body struct {
				WorkspacePath string `json:"workspacePath"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.WorkspacePath == "" {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if err := reg.Disable(id, body.WorkspacePath); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)

		default:
			http.NotFound(w, r)
		}
	})
}

func jsonResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
