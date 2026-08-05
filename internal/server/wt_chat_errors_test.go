package server

import (
	"errors"
	"fmt"
	"testing"

	"github.com/piaobeizu/tether/internal/session"
	"github.com/piaobeizu/tether/internal/wire"
)

// TestErrorEnvelope_UnwrapsARefusalsOwnCode pins that errorEnvelope recovers
// the daemon-decided wire.ErrorCode from a session.Refusal via errors.As,
// even when it is wrapped by an intermediate %w (as resolveWorkspace's,
// spawnEntry's, and resolve's refusals all are through refuse()).
func TestErrorEnvelope_UnwrapsARefusalsOwnCode(t *testing.T) {
	base := &session.Refusal{Code: wire.ErrCodeUnknownWorkspace, Err: errors.New(`unknown workspace "foo"`)}
	wrapped := fmt.Errorf("attach: %w", base)

	env := errorEnvelope(wrapped)
	if env.Kind != wire.KindError {
		t.Fatalf("Kind = %q, want %q", env.Kind, wire.KindError)
	}
	payload, ok := env.Payload.(wire.ErrorPayload)
	if !ok {
		t.Fatalf("Payload = %T, want wire.ErrorPayload", env.Payload)
	}
	if payload.Code != wire.ErrCodeUnknownWorkspace {
		t.Errorf("Code = %q, want %q", payload.Code, wire.ErrCodeUnknownWorkspace)
	}
	if !payload.Terminal {
		t.Error("ErrCodeUnknownWorkspace must be terminal")
	}
	// The MESSAGE must be the WHOLE error, not the innermost Refusal's text
	// (review W1). errors.As finds the inner Refusal, so reading its Error()
	// would silently drop every wrapping layer — and Attachment.resolve wraps
	// with the half that says which recovery was being attempted
	// ("resume %s failed and fresh spawn failed: %w").
	if want := `attach: unknown workspace "foo"`; payload.Message != want {
		t.Errorf("Message = %q, want %q — the wrapping context must survive", payload.Message, want)
	}
}

// TestErrorEnvelope_UnclassifiedErrorDefaultsRetryable pins the fallback for
// an error errorEnvelope does not recognize as a session.Refusal: it must
// still produce a usable KindError envelope, with Terminal defaulting to
// false (see wire.ErrorCode.Terminal's doc comment) rather than the call
// panicking or the connection being bricked on a code nobody assigned.
func TestErrorEnvelope_UnclassifiedErrorDefaultsRetryable(t *testing.T) {
	env := errorEnvelope(errors.New("some error this package was never taught to classify"))
	payload, ok := env.Payload.(wire.ErrorPayload)
	if !ok {
		t.Fatalf("Payload = %T, want wire.ErrorPayload", env.Payload)
	}
	if payload.Message != "some error this package was never taught to classify" {
		t.Errorf("Message = %q, want the error's own text", payload.Message)
	}
	if payload.Terminal {
		t.Error("an unclassified error must default to retryable (Terminal=false), not terminal")
	}
}
