package lifecycle

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/piaobeizu/tether/internal/mcp/host"
)

// taskConfigFile is the DTO for <wsRoot>/.tether/task-config.json.
// It exists to bind snake_case JSON keys that host.ServerConfig (which has no
// json tags) cannot decode directly.
type taskConfigFile struct {
	Version int                        `json:"version"`
	Servers map[string]serverConfigDTO `json:"servers"`
}

// serverConfigDTO mirrors host.ServerConfig with explicit json tags.
type serverConfigDTO struct {
	Command    []string          `json:"command"`
	Env        map[string]string `json:"env"`
	Prefix     string            `json:"prefix"`
	InheritEnv []string          `json:"inherit_env"`
}

// LoadTaskConfig reads <wsRoot>/.tether/task-config.json and returns the
// configured extra MCP servers keyed by server name.
//
// A missing file is not an error: it returns (nil, nil). It returns a
// descriptive error on malformed JSON, an unsupported version, or any server
// with an empty command.
func LoadTaskConfig(wsRoot string) (map[string]host.ServerConfig, error) {
	path := filepath.Join(wsRoot, ".tether", "task-config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("task-config: read %s: %w", path, err)
	}

	var file taskConfigFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("task-config: parse %s: %w", path, err)
	}
	if file.Version != 1 {
		return nil, fmt.Errorf("task-config: unsupported version %d (want 1)", file.Version)
	}

	if len(file.Servers) == 0 {
		return nil, nil
	}

	out := make(map[string]host.ServerConfig, len(file.Servers))
	for name, dto := range file.Servers {
		if len(dto.Command) == 0 {
			return nil, fmt.Errorf("task-config: server %q has empty command", name)
		}
		out[name] = host.ServerConfig{
			Command:    dto.Command,
			Env:        dto.Env,
			Prefix:     dto.Prefix,
			InheritEnv: dto.InheritEnv,
		}
	}
	return out, nil
}
