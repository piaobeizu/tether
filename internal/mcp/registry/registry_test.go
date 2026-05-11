// internal/mcp/registry/registry_test.go
package registry_test

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/piaobeizu/tether/internal/mcp/host"
	"github.com/piaobeizu/tether/internal/mcp/registry"
)

func tools(names ...string) []mcp.Tool {
	out := make([]mcp.Tool, len(names))
	for i, n := range names {
		out[i] = mcp.Tool{Name: n}
	}
	return out
}

func TestRegistryRegisterAndLookup(t *testing.T) {
	r := registry.New()
	cfg := host.ServerConfig{Name: "pf2-coding"}
	if err := r.Register(cfg, tools("commit", "diff")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	srvName, origName, ok := r.Lookup("pf2_coding_commit")
	if !ok || srvName != "pf2-coding" || origName != "commit" {
		t.Fatalf("Lookup: got (%q,%q,%v)", srvName, origName, ok)
	}
}

func TestRegistryListAll(t *testing.T) {
	r := registry.New()
	r.Register(host.ServerConfig{Name: "jira"}, tools("create_issue", "search"))
	entries := r.ListAll()
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.PrefixedName] = true
	}
	if !names["jira_create_issue"] || !names["jira_search"] {
		t.Fatalf("unexpected prefixed names: %v", names)
	}
}

func TestRegistryDeregister(t *testing.T) {
	r := registry.New()
	r.Register(host.ServerConfig{Name: "svc"}, tools("foo"))
	r.Deregister("svc")
	if _, _, ok := r.Lookup("svc_foo"); ok {
		t.Fatal("after Deregister, Lookup must return ok=false")
	}
	if len(r.ListAll()) != 0 {
		t.Fatal("ListAll after Deregister must be empty")
	}
}

func TestRegistryCollisionReturnsError(t *testing.T) {
	r := registry.New()
	cfgA := host.ServerConfig{Name: "pf2-coding", Prefix: "pf2_coding"}
	cfgB := host.ServerConfig{Name: "other", Prefix: "pf2_coding"} // same effective prefix
	r.Register(cfgA, tools("commit"))
	if err := r.Register(cfgB, tools("commit")); err == nil {
		t.Fatal("expected collision error, got nil")
	}
}

func TestRegistryLookupServerByPrefixedName(t *testing.T) {
	r := registry.New()
	r.Register(host.ServerConfig{Name: "jira"}, tools("get_issue"))
	srvName, ok := r.LookupServer("jira_get_issue")
	if !ok || srvName != "jira" {
		t.Fatalf("LookupServer: got (%q,%v)", srvName, ok)
	}
}

func TestRegistryListAllEmptyAfterDeregister(t *testing.T) {
	r := registry.New()
	r.Register(host.ServerConfig{Name: "alpha"}, tools("a"))
	r.Register(host.ServerConfig{Name: "beta"}, tools("b"))
	r.Deregister("alpha")
	entries := r.ListAll()
	if len(entries) != 1 || entries[0].ServerName != "beta" {
		t.Fatalf("expected only beta after deregister alpha: %+v", entries)
	}
}
