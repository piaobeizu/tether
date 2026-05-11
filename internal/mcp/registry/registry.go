// internal/mcp/registry/registry.go
package registry

import (
	"fmt"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/piaobeizu/tether/internal/mcp/host"
)

// ToolEntry is a registered tool with routing metadata.
type ToolEntry struct {
	PrefixedName string
	OriginalName string
	ServerName   string
	Tool         mcp.Tool
}

// Registry maps prefixed tool names to their server and original name.
// Register applies NamespacePrefix; callers pass raw tool lists.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]*ToolEntry // prefixed name → entry
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{tools: make(map[string]*ToolEntry)}
}

// Register adds tools from serverCfg, applying namespace prefix.
// Returns an error if any prefixed name already exists (no silent override).
func (r *Registry) Register(serverCfg host.ServerConfig, tools []mcp.Tool) error {
	prefix := host.NamespacePrefix(serverCfg)
	r.mu.Lock()
	defer r.mu.Unlock()
	// Collision check first (all-or-nothing).
	for _, t := range tools {
		prefixed := prefix + t.Name
		if existing, ok := r.tools[prefixed]; ok {
			return fmt.Errorf("mcp/registry: tool %q already registered by server %q",
				prefixed, existing.ServerName)
		}
	}
	for _, t := range tools {
		prefixed := prefix + t.Name
		r.tools[prefixed] = &ToolEntry{
			PrefixedName: prefixed,
			OriginalName: t.Name,
			ServerName:   serverCfg.Name,
			Tool:         t,
		}
	}
	return nil
}

// Deregister removes all tools registered under serverName.
func (r *Registry) Deregister(serverName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, e := range r.tools {
		if e.ServerName == serverName {
			delete(r.tools, k)
		}
	}
}

// Lookup returns the server name and original tool name for a prefixed tool.
func (r *Registry) Lookup(prefixedName string) (serverName, originalName string, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e, found := r.tools[prefixedName]; found {
		return e.ServerName, e.OriginalName, true
	}
	return "", "", false
}

// LookupServer returns just the server name for a prefixed tool name.
func (r *Registry) LookupServer(prefixedName string) (serverName string, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e, found := r.tools[prefixedName]; found {
		return e.ServerName, true
	}
	return "", false
}

// ListAll returns a snapshot of all registered tools.
func (r *Registry) ListAll() []ToolEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ToolEntry, 0, len(r.tools))
	for _, e := range r.tools {
		out = append(out, *e)
	}
	return out
}
