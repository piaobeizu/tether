package lifecycle_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/piaobeizu/tether/internal/mcp/lifecycle"
)

// writeTaskConfig writes the given JSON body to <dir>/.tether/task-config.json.
func writeTaskConfig(t *testing.T, dir, body string) {
	t.Helper()
	td := filepath.Join(dir, ".tether")
	if err := os.MkdirAll(td, 0o755); err != nil {
		t.Fatalf("mkdir .tether: %v", err)
	}
	if err := os.WriteFile(filepath.Join(td, "task-config.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write task-config.json: %v", err)
	}
}

func TestLoadTaskConfig_MissingFile(t *testing.T) {
	dir := t.TempDir()
	got, err := lifecycle.LoadTaskConfig(dir)
	if err != nil {
		t.Fatalf("missing file should not error, got: %v", err)
	}
	if got != nil {
		t.Fatalf("missing file should return nil map, got: %v", got)
	}
}

func TestLoadTaskConfig_ValidSingle(t *testing.T) {
	dir := t.TempDir()
	writeTaskConfig(t, dir, `{
		"version": 1,
		"servers": {
			"foo": {
				"command": ["foo-bin", "--flag"],
				"env": {"K": "V"},
				"prefix": "fx",
				"inherit_env": ["GOPATH", "HOME"]
			}
		}
	}`)

	got, err := lifecycle.LoadTaskConfig(dir)
	if err != nil {
		t.Fatalf("valid config error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 server, got %d", len(got))
	}
	sc, ok := got["foo"]
	if !ok {
		t.Fatalf("expected server 'foo', got %v", got)
	}
	if len(sc.Command) != 2 || sc.Command[0] != "foo-bin" || sc.Command[1] != "--flag" {
		t.Fatalf("command not mapped: %v", sc.Command)
	}
	if sc.Env["K"] != "V" {
		t.Fatalf("env not mapped: %v", sc.Env)
	}
	if sc.Prefix != "fx" {
		t.Fatalf("prefix not mapped: %q", sc.Prefix)
	}
	if len(sc.InheritEnv) != 2 || sc.InheritEnv[0] != "GOPATH" || sc.InheritEnv[1] != "HOME" {
		t.Fatalf("inherit_env not mapped: %v", sc.InheritEnv)
	}
}

func TestLoadTaskConfig_ValidMulti(t *testing.T) {
	dir := t.TempDir()
	writeTaskConfig(t, dir, `{
		"version": 1,
		"servers": {
			"a": {"command": ["a-bin"]},
			"b": {"command": ["b-bin", "arg"]}
		}
	}`)

	got, err := lifecycle.LoadTaskConfig(dir)
	if err != nil {
		t.Fatalf("valid multi config error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(got))
	}
	if _, ok := got["a"]; !ok {
		t.Fatalf("missing server 'a': %v", got)
	}
	if _, ok := got["b"]; !ok {
		t.Fatalf("missing server 'b': %v", got)
	}
}

func TestLoadTaskConfig_Errors(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"malformed JSON", `{"version": 1, "servers": {`},
		{"version 0", `{"version": 0, "servers": {"a": {"command": ["a-bin"]}}}`},
		{"version 2", `{"version": 2, "servers": {"a": {"command": ["a-bin"]}}}`},
		{"empty command", `{"version": 1, "servers": {"a": {"command": []}}}`},
		{"missing command", `{"version": 1, "servers": {"a": {"env": {"K": "V"}}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTaskConfig(t, dir, tc.body)
			_, err := lifecycle.LoadTaskConfig(dir)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}
