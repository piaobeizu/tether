package server

import (
	"testing"

	"github.com/piaobeizu/tether/internal/wire"
)

func TestRespondToControl_Ping(t *testing.T) {
	f := wire.ClientFrame{Kind: wire.ClientFramePing, TS: 1234567890}
	resp, ok := RespondToControl(f)
	if !ok {
		t.Fatal("expected ok=true for ping frame")
	}
	if resp == nil {
		t.Fatal("expected non-nil ControlFrame for ping")
	}
	if resp.Kind != wire.ControlPong {
		t.Fatalf("resp.Kind = %q, want %q", resp.Kind, wire.ControlPong)
	}
	if resp.TS != f.TS {
		t.Fatalf("resp.TS = %d, want %d (echoed)", resp.TS, f.TS)
	}
}

func TestRespondToControl_PingZeroTS(t *testing.T) {
	f := wire.ClientFrame{Kind: wire.ClientFramePing, TS: 0}
	resp, ok := RespondToControl(f)
	if !ok {
		t.Fatal("expected ok=true for ping frame with ts=0")
	}
	if resp.TS != 0 {
		t.Fatalf("resp.TS = %d, want 0", resp.TS)
	}
}

func TestRespondToControl_UnknownKind(t *testing.T) {
	f := wire.ClientFrame{Kind: wire.ClientFrameAction, Action: "approve", BlockID: "b1"}
	resp, ok := RespondToControl(f)
	if ok {
		t.Fatal("expected ok=false for non-ping frame")
	}
	if resp != nil {
		t.Fatalf("expected nil ControlFrame, got %+v", resp)
	}
}

func TestRespondToControl_EmptyKind(t *testing.T) {
	f := wire.ClientFrame{}
	resp, ok := RespondToControl(f)
	if ok || resp != nil {
		t.Fatalf("expected (nil, false) for empty frame, got (%+v, %v)", resp, ok)
	}
}
