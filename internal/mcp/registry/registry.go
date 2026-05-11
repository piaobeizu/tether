// internal/mcp/registry/registry.go
package registry

import (
	"fmt"
	"log/slog"
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

// RegistryEvent describes a tool-set change for a single server.
type RegistryEvent struct {
	Server  string
	Added   []ToolEntry
	Removed []ToolEntry
}

// Observer is a callback invoked after Register or Deregister mutates the
// registry. Observers must be non-blocking. Panics are recovered and logged;
// one bad observer must not prevent siblings from firing.
type Observer func(RegistryEvent)

// Registry maps prefixed tool names to their server and original name.
type Registry struct {
	mu        sync.RWMutex
	tools     map[string]*ToolEntry
	observers []Observer
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{tools: make(map[string]*ToolEntry)}
}

// AddObserver registers fn to be invoked on every Register / Deregister,
// after the write lock has been released.
func (r *Registry) AddObserver(fn Observer) {
	r.mu.Lock()
	r.observers = append(r.observers, fn)
	r.mu.Unlock()
}

// Register adds tools from serverCfg, applying namespace prefix.
// Returns an error if any prefixed name already exists (no silent override).
func (r *Registry) Register(serverCfg host.ServerConfig, tools []mcp.Tool) error {
	prefix := host.NamespacePrefix(serverCfg)

	r.mu.Lock()
	for _, t := range tools {
		prefixed := prefix + t.Name
		if existing, ok := r.tools[prefixed]; ok {
			r.mu.Unlock()
			return fmt.Errorf("mcp/registry: tool %q already registered by server %q",
				prefixed, existing.ServerName)
		}
	}
	added := make([]ToolEntry, 0, len(tools))
	for _, t := range tools {
		prefixed := prefix + t.Name
		entry := &ToolEntry{
			PrefixedName: prefixed,
			OriginalName: t.Name,
			ServerName:   serverCfg.Name,
			Tool:         t,
		}
		r.tools[prefixed] = entry
		added = append(added, *entry)
	}
	obsSnapshot := append([]Observer(nil), r.observers...)
	r.mu.Unlock()

	if len(added) > 0 {
		notify(obsSnapshot, RegistryEvent{Server: serverCfg.Name, Added: added})
	}
	return nil
}

// Deregister removes all tools registered under serverName.
func (r *Registry) Deregister(serverName string) {
	r.mu.Lock()
	var removed []ToolEntry
	for k, e := range r.tools {
		if e.ServerName == serverName {
			removed = append(removed, *e)
			delete(r.tools, k)
		}
	}
	obsSnapshot := append([]Observer(nil), r.observers...)
	r.mu.Unlock()

	if len(removed) > 0 {
		notify(obsSnapshot, RegistryEvent{Server: serverName, Removed: removed})
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

func notify(observers []Observer, e RegistryEvent) {
	for _, fn := range observers {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					slog.Error("mcp/registry: observer panicked", "panic", rec)
				}
			}()
			fn(e)
		}()
	}
}
