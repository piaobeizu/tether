package server

import (
	"errors"
	"fmt"
	"strings"
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

// TestPromptErrorEnvelope_OnlySpeaksUpForARecoveryThatFailed pins the discriminator
// serveChat's prompt reader uses on a failed SendPrompt (tether#59).
//
// Both sides matter and they fail in opposite directions:
//
//   - The failed-resume send is EXPECTED and self-healing (Attachment.resolve replays
//     it onto a fresh session), and it arrives as a bare transport error. Sending an
//     envelope for it would show the user an error for a turn that then answers
//     normally, and would clear the browser's turn state mid-flight — store.ts's
//     'error' branch resets streaming unconditionally.
//   - A re-open the daemon could not complete carries a classified Refusal, nothing
//     will retry it, and the attachment's one re-open is spent. Silence there is the
//     tether#59 hang one step further out: the user watches a spinner while every
//     later prompt fails into the log.
func TestPromptErrorEnvelope_OnlySpeaksUpForARecoveryThatFailed(t *testing.T) {
	t.Run("bare transport error → say nothing", func(t *testing.T) {
		// The shape a cc stdin write actually produces on the failed-resume path.
		if env, ok := promptErrorEnvelope(errors.New("write |1: broken pipe")); ok {
			t.Errorf("promptErrorEnvelope reported %+v for a send that Resolve recovers on its own", env)
		}
	})

	t.Run("classified re-open failure → send it, code and words intact", func(t *testing.T) {
		// As Attachment.reopen builds it: spawnEntry's Refusal, wrapped in the message
		// that names the recovery being attempted.
		inner := &session.Refusal{Code: wire.ErrCodeSpawnFailed, Err: errors.New(`spawn: exec: "cc": executable file not found in $PATH`)}
		err := fmt.Errorf("reused session abc stopped accepting prompts (write |1: broken pipe) and could not be re-opened: %w", inner)

		env, ok := promptErrorEnvelope(err)
		if !ok {
			t.Fatal("promptErrorEnvelope stayed silent about a recovery the daemon knows failed")
		}
		if env.Kind != wire.KindError {
			t.Fatalf("Kind = %q, want %q", env.Kind, wire.KindError)
		}
		payload, okp := env.Payload.(wire.ErrorPayload)
		if !okp {
			t.Fatalf("Payload = %T, want wire.ErrorPayload", env.Payload)
		}
		if payload.Code != wire.ErrCodeSpawnFailed {
			t.Errorf("Code = %q, want %q", payload.Code, wire.ErrCodeSpawnFailed)
		}
		if payload.Terminal {
			t.Error("Terminal = true; a re-open failure must stay retryable — the next reconnect re-resumes")
		}
		// The whole wrapped message, not the innermost Refusal's: a browser told only
		// "spawn: ..." has lost the half that says which recovery was being attempted.
		for _, want := range []string{"stopped accepting prompts", "could not be re-opened", "executable file not found"} {
			if !strings.Contains(payload.Message, want) {
				t.Errorf("message %q does not mention %q", payload.Message, want)
			}
		}
	})
}
