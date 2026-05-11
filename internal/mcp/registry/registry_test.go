// internal/mcp/registry/registry_test.go
package registry_test

import (
	"sync"
	"sync/atomic"
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

func TestRegistryAddObserverFiresOnRegisterAndDeregister(t *testing.T) {
	r := registry.New()
	var events []registry.RegistryEvent
	var mu sync.Mutex
	r.AddObserver(func(e registry.RegistryEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	})

	r.Register(host.ServerConfig{Name: "svc"}, tools("a", "b"))
	r.Deregister("svc")

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Server != "svc" || len(events[0].Added) != 2 || len(events[0].Removed) != 0 {
		t.Fatalf("event[0]: %+v", events[0])
	}
	if events[1].Server != "svc" || len(events[1].Added) != 0 || len(events[1].Removed) != 2 {
		t.Fatalf("event[1]: %+v", events[1])
	}
}

func TestRegistryReconnectPattern_FiresAddsAndRemoves(t *testing.T) {
	r := registry.New()
	var events []registry.RegistryEvent
	r.AddObserver(func(e registry.RegistryEvent) { events = append(events, e) })

	if err := r.Register(host.ServerConfig{Name: "svc"}, tools("a", "b")); err != nil {
		t.Fatal(err)
	}
	r.Deregister("svc")
	if err := r.Register(host.ServerConfig{Name: "svc"}, tools("b", "c")); err != nil {
		t.Fatal(err)
	}

	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d: %+v", len(events), events)
	}
	if events[0].Server != "svc" || len(events[0].Added) != 2 || len(events[0].Removed) != 0 {
		t.Fatalf("event[0] (initial register): %+v", events[0])
	}
	if events[1].Server != "svc" || len(events[1].Added) != 0 || len(events[1].Removed) != 2 {
		t.Fatalf("event[1] (deregister): %+v", events[1])
	}
	if events[2].Server != "svc" || len(events[2].Added) != 2 || len(events[2].Removed) != 0 {
		t.Fatalf("event[2] (re-register): %+v", events[2])
	}
	addedNames := map[string]bool{}
	for _, e := range events[2].Added {
		addedNames[e.PrefixedName] = true
	}
	if !addedNames["svc_b"] || !addedNames["svc_c"] {
		t.Fatalf("event[2] Added must include svc_b + svc_c, got %v", addedNames)
	}
}

func TestRegistryRegister_CollisionOnExistingServerErrors(t *testing.T) {
	r := registry.New()
	if err := r.Register(host.ServerConfig{Name: "svc"}, tools("a")); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(host.ServerConfig{Name: "svc"}, tools("a")); err == nil {
		t.Fatal("expected collision error on second Register of same server")
	}
}

func TestRegistryObserverFanOut(t *testing.T) {
	r := registry.New()
	var c1, c2 atomic.Int32
	r.AddObserver(func(e registry.RegistryEvent) { c1.Add(1) })
	r.AddObserver(func(e registry.RegistryEvent) { c2.Add(1) })
	r.Register(host.ServerConfig{Name: "svc"}, tools("a"))
	if c1.Load() != 1 || c2.Load() != 1 {
		t.Fatalf("both observers must fire: c1=%d c2=%d", c1.Load(), c2.Load())
	}
}

func TestRegistryObserverPanicIsIsolated(t *testing.T) {
	r := registry.New()
	var goodFired atomic.Int32
	r.AddObserver(func(e registry.RegistryEvent) { panic("boom") })
	r.AddObserver(func(e registry.RegistryEvent) { goodFired.Add(1) })
	r.Register(host.ServerConfig{Name: "svc"}, tools("a"))
	if goodFired.Load() != 1 {
		t.Fatalf("good observer must fire even when prior observer panics: %d", goodFired.Load())
	}
}

func TestRegistryObserverFiresAfterLockRelease(t *testing.T) {
	r := registry.New()
	r.AddObserver(func(e registry.RegistryEvent) {
		// If the write lock were still held, ListAll (which RLocks) would deadlock.
		_ = r.ListAll()
	})
	r.Register(host.ServerConfig{Name: "svc"}, tools("a"))
	// Reaching here without timeout proves observers run outside the write lock.
}
