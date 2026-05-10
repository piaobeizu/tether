package permission_test

import (
	"testing"

	"github.com/piaobeizu/tether/internal/permission"
)

func TestPermStateRoundTrip(t *testing.T) {
	ps := permission.NewPermState()
	req := &permission.PermRequest{ID: "x1", ToolName: "Bash"}
	ch, err := ps.Add(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ps.Decide("x1", false) })
	if !ps.Decide("x1", true) {
		t.Fatal("Decide returned false for known id")
	}
	if allow := <-ch; !allow {
		t.Fatal("expected allow=true")
	}
}

func TestPermStateUnknownID(t *testing.T) {
	ps := permission.NewPermState()
	if ps.Decide("no-such-id", true) {
		t.Fatal("Decide should return false for unknown id")
	}
}

func TestPermStateDuplicateID(t *testing.T) {
	ps := permission.NewPermState()
	req := &permission.PermRequest{ID: "dup1", ToolName: "Bash"}
	_, err := ps.Add(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ps.Decide("dup1", false) })
	_, err = ps.Add(&permission.PermRequest{ID: "dup1", ToolName: "Write"})
	if err == nil {
		t.Fatal("expected error for duplicate ID, got nil")
	}
}
