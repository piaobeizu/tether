// internal/permission/manager_test.go
package permission_test

import (
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/permission"
)

func TestManagerAddDecideAllow(t *testing.T) {
	m := permission.New()
	req := &permission.Request{Source: "claude_hook", ToolName: "bash", Args: []byte(`{}`)}
	ch := m.Add(req)
	if req.ID == "" {
		t.Fatal("Add must set req.ID")
	}
	go func() { m.Decide(req.ID, true, "") }()
	dec := <-ch
	if !dec.Allow {
		t.Fatalf("expected Allow=true, got %+v", dec)
	}
}

func TestManagerAddDecideDeny(t *testing.T) {
	m := permission.New()
	req := &permission.Request{Source: "mcp:polyforge-coding", ToolName: "commit"}
	ch := m.Add(req)
	go func() { m.Decide(req.ID, false, "user denied") }()
	dec := <-ch
	if dec.Allow || dec.Reason != "user denied" {
		t.Fatalf("expected deny with reason, got %+v", dec)
	}
}

func TestManagerTimeout(t *testing.T) {
	oldTimeout := permission.Timeout
	permission.Timeout = 50 * time.Millisecond
	defer func() { permission.Timeout = oldTimeout }()

	m := permission.New()
	req := &permission.Request{Source: "claude_hook", ToolName: "write"}
	ch := m.Add(req)
	dec := <-ch
	if dec.Allow {
		t.Fatal("timeout must produce Allow=false")
	}
}

func TestManagerDecideUnknownID(t *testing.T) {
	m := permission.New()
	if m.Decide("nonexistent", true, "") {
		t.Fatal("Decide on unknown ID must return false")
	}
}

func TestManagerGetPending(t *testing.T) {
	m := permission.New()
	req := &permission.Request{Source: "claude_hook", ToolName: "read"}
	m.Add(req)
	got := m.GetPending(req.ID)
	if got == nil || got.ToolName != "read" {
		t.Fatalf("GetPending returned wrong req: %+v", got)
	}
	m.Decide(req.ID, true, "")
	if m.GetPending(req.ID) != nil {
		t.Fatal("GetPending after Decide must return nil")
	}
}
