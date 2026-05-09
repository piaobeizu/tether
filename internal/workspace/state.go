// Package workspace manages the workspace registry stored in ~/.tether/workspaces.toml.
package workspace

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Workspace represents a single registered workspace entry.
type Workspace struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	AddedAt   time.Time `json:"addedAt"`
	ActiveSID string    `json:"activeSid,omitempty"` // last active cc session ID
}

// Registry manages the in-memory + persisted workspace list.
type Registry struct {
	mu         sync.RWMutex
	workspaces []Workspace
	path       string // resolved path to workspaces.toml (stored as JSON for simplicity)
}

// NewRegistry loads (or creates) the workspace registry from ~/.tether/workspaces.json.
func NewRegistry() (*Registry, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".tether")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir ~/.tether: %w", err)
	}
	path := filepath.Join(dir, "workspaces.json")
	r := &Registry{path: path}
	_ = r.load() // ignore if absent
	return r, nil
}

// List returns a snapshot of all registered workspaces.
func (r *Registry) List() []Workspace {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Workspace, len(r.workspaces))
	copy(out, r.workspaces)
	return out
}

// Add registers a new workspace (deduplicated by path).
func (r *Registry) Add(name, path string) (Workspace, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Workspace{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, w := range r.workspaces {
		if w.Path == abs {
			return w, nil
		}
	}
	w := Workspace{
		ID:      newID(),
		Name:    name,
		Path:    abs,
		AddedAt: time.Now().UTC(),
	}
	r.workspaces = append(r.workspaces, w)
	return w, r.saveLocked()
}

// Remove removes a workspace by ID.
func (r *Registry) Remove(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, w := range r.workspaces {
		if w.ID != id {
			r.workspaces[n] = w
			n++
		}
	}
	r.workspaces = r.workspaces[:n]
	return r.saveLocked()
}

func (r *Registry) load() error {
	data, err := os.ReadFile(r.path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &r.workspaces)
}

func (r *Registry) saveLocked() error {
	b, err := json.MarshalIndent(r.workspaces, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
