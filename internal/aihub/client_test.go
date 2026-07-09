package aihub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- LoadConfig ----

func TestLoadConfig_EnvOnly(t *testing.T) {
	t.Setenv("TETHER_AIHUB_URL", "http://aihub.example.com")
	t.Setenv("TETHER_AIHUB_KEY", "secret-key")

	baseURL, key, ok := LoadConfig()
	if !ok {
		t.Fatalf("LoadConfig() ok = false, want true")
	}
	if baseURL != "http://aihub.example.com" {
		t.Errorf("baseURL = %q, want %q", baseURL, "http://aihub.example.com")
	}
	if key != "secret-key" {
		t.Errorf("key = %q, want %q", key, "secret-key")
	}
}

func TestLoadConfigFrom_TomlFallback(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	content := "machine_id = \"abc-123\"\n" +
		"[auth]\n" +
		"api_key = \"toml-key\"\n" +
		"[server]\n" +
		"url = \"http://toml-host\"\n"
	if err := os.WriteFile(tomlPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp toml: %v", err)
	}

	noEnv := func(string) (string, bool) { return "", false }
	baseURL, key, ok := loadConfigFrom(noEnv, tomlPath)
	if !ok {
		t.Fatalf("loadConfigFrom() ok = false, want true")
	}
	if baseURL != "http://toml-host" {
		t.Errorf("baseURL = %q, want %q", baseURL, "http://toml-host")
	}
	if key != "toml-key" {
		t.Errorf("key = %q, want %q", key, "toml-key")
	}
}

func TestLoadConfigFrom_PartialEnvFallsBackToToml(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	content := "[auth]\napi_key = \"toml-key\"\n[server]\nurl = \"http://toml-host\"\n"
	if err := os.WriteFile(tomlPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp toml: %v", err)
	}

	// Env only provides the URL; key must come from the toml fallback.
	env := func(k string) (string, bool) {
		if k == "TETHER_AIHUB_URL" {
			return "http://env-host", true
		}
		return "", false
	}
	baseURL, key, ok := loadConfigFrom(env, tomlPath)
	if !ok {
		t.Fatalf("loadConfigFrom() ok = false, want true")
	}
	if baseURL != "http://env-host" {
		t.Errorf("baseURL = %q, want env value %q", baseURL, "http://env-host")
	}
	if key != "toml-key" {
		t.Errorf("key = %q, want toml fallback %q", key, "toml-key")
	}
}

func TestLoadConfigFrom_NeitherSource(t *testing.T) {
	noEnv := func(string) (string, bool) { return "", false }
	baseURL, key, ok := loadConfigFrom(noEnv, filepath.Join(t.TempDir(), "missing.toml"))
	if ok {
		t.Fatalf("loadConfigFrom() ok = true, want false when neither env nor toml has creds")
	}
	if baseURL != "" || key != "" {
		t.Errorf("expected empty baseURL/key on ok=false, got baseURL=%q key=%q", baseURL, key)
	}
}

func TestLoadConfigFrom_MissingTomlFileNoPanic(t *testing.T) {
	noEnv := func(string) (string, bool) { return "", false }
	// Path doesn't exist at all; must not error or panic.
	_, _, ok := loadConfigFrom(noEnv, "/nonexistent/path/config.toml")
	if ok {
		t.Fatalf("expected ok=false for a missing toml file")
	}
}

// ---- getJSON ----

func TestGetJSON_Success(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"hello": "world"})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-key")
	var out map[string]string
	if err := c.getJSON(context.Background(), "/anything", &out); err != nil {
		t.Fatalf("getJSON: %v", err)
	}
	if out["hello"] != "world" {
		t.Errorf("out = %v, want hello=world", out)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer test-key")
	}
}

func TestGetJSON_Forbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-key")
	var out struct{}
	err := c.getJSON(context.Background(), "/anything", &out)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("getJSON error = %v, want errors.Is(err, ErrForbidden)", err)
	}
}

func TestGetJSON_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-key")
	var out struct{}
	err := c.getJSON(context.Background(), "/anything", &out)
	if err == nil {
		t.Fatalf("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not mention status 500", err.Error())
	}
}

// ---- Typed methods ----

func TestReadyQueue(t *testing.T) {
	const canned = `{
		"items": [
			{"id":"wi_1","slug":"myproj-1","wi_type":"feature","priority":"high","goal":"do the thing","created_at":"2026-01-01T00:00:00Z"}
		],
		"running": [],
		"stalled": [],
		"paused": [],
		"needs_human_session": [],
		"unclassified": []
	}`

	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(canned))
	}))
	defer srv.Close()

	c := New(srv.URL, "test-key")
	rq, err := c.ReadyQueue(context.Background(), "myproj", 25)
	if err != nil {
		t.Fatalf("ReadyQueue: %v", err)
	}

	if len(rq.Items) != 1 {
		t.Fatalf("Items = %+v, want 1 item", rq.Items)
	}
	item := rq.Items[0]
	if item.ID != "wi_1" || item.Slug != "myproj-1" || item.Goal != "do the thing" {
		t.Errorf("Items[0] = %+v, unexpected fields", item)
	}
	if item.WIType == nil || *item.WIType != "feature" {
		t.Errorf("Items[0].WIType = %v, want \"feature\"", item.WIType)
	}
	if item.Priority != "high" {
		t.Errorf("Items[0].Priority = %q, want high", item.Priority)
	}

	if gotPath != "/v1/work_items/ready" {
		t.Errorf("request path = %q, want %q", gotPath, "/v1/work_items/ready")
	}
	q, err := url.ParseQuery(gotQuery)
	if err != nil {
		t.Fatalf("parse query %q: %v", gotQuery, err)
	}
	if q.Get("project") != "myproj" {
		t.Errorf("query project = %q, want myproj", q.Get("project"))
	}
	if q.Get("max") != "25" {
		t.Errorf("query max = %q, want 25", q.Get("max"))
	}
}

func TestProjects_UnwrapsItemsEnvelope(t *testing.T) {
	// aihub GET /v1/projects returns {"items":[...]}, not a bare array — the
	// client must unwrap it (the one typed method with custom decode logic).
	const canned = `{"items":[{"name":"tether"},{"name":"aihub"}]}`
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(canned))
	}))
	defer srv.Close()

	c := New(srv.URL, "test-key")
	projs, err := c.Projects(context.Background())
	if err != nil {
		t.Fatalf("Projects: %v", err)
	}
	if len(projs) != 2 || projs[0].Name != "tether" || projs[1].Name != "aihub" {
		t.Fatalf("Projects = %+v, want [tether aihub] unwrapped from items envelope", projs)
	}
	if gotPath != "/v1/projects" {
		t.Errorf("request path = %q, want /v1/projects", gotPath)
	}
}
