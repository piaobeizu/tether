// internal/mcp/registry/namespace_test.go
package registry_test

import (
	"testing"

	"github.com/piaobeizu/tether/internal/mcp/host"
)

func TestNamespacePrefixDefault(t *testing.T) {
	cfg := host.ServerConfig{Name: "polyforge-coding"}
	got := host.NamespacePrefix(cfg)
	if got != "polyforge_coding_" {
		t.Fatalf("want polyforge_coding_, got %q", got)
	}
}

func TestNamespacePrefixOverride(t *testing.T) {
	cfg := host.ServerConfig{Name: "polyforge-coding", Prefix: "pf2_coding"}
	got := host.NamespacePrefix(cfg)
	if got != "pf2_coding_" {
		t.Fatalf("want pf2_coding_, got %q", got)
	}
}

func TestNamespacePrefixSimple(t *testing.T) {
	cfg := host.ServerConfig{Name: "jira"}
	got := host.NamespacePrefix(cfg)
	if got != "jira_" {
		t.Fatalf("want jira_, got %q", got)
	}
}
