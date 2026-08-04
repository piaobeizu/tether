package session

import (
	"fmt"

	"github.com/piaobeizu/tether/internal/wire"
)

// Refusal is a typed daemon-side rejection carrying the wire.ErrorCode a
// caller (internal/server/wt_chat.go) should surface to the browser.
//
// # Why this does NOT live in internal/wire
//
// wire is scanned by tygo (tygo.yaml) to generate web/src/lib/wire.gen.ts —
// doc.go's rule is "only types that cross the wire belong here", and
// everything EXPORTED in that package becomes a TypeScript type whether or
// not that makes sense on the browser side. Refusal wraps a Go `error`
// (Err), which has no sane JSON/TypeScript shape tygo could emit — the field
// is a Go interface value, not data. Keeping Refusal here, one import away
// from wire, is what keeps a nonsensical type out of wire.gen.ts. What
// actually crosses the wire is wire.ErrorPayload, built from this type's Code
// by wire.NewErrorEnvelope (see internal/server/wt_chat.go's errorEnvelope) —
// Refusal itself never gets near json.Marshal.
type Refusal struct {
	Code wire.ErrorCode
	Err  error
}

// Error satisfies the error interface by delegating to the wrapped error's
// text, so a Refusal reads exactly like the plain error it replaces at every
// existing call site (slog.Warn("...", "err", err) and the like keep working
// unchanged).
func (e *Refusal) Error() string {
	return e.Err.Error()
}

// Unwrap exposes the underlying error to errors.Is/errors.As, so a Refusal
// wrapping a context error (e.g. ErrCodeConnectionClosed wrapping ctx.Err())
// still lets a caller test for that underlying error if it ever needs to.
func (e *Refusal) Unwrap() error {
	return e.Err
}

// refuse builds a Refusal in the same fmt.Errorf-with-%w idiom every error in
// this package already used before tether#63 — so wrapping an existing
// `return nil, fmt.Errorf(...)` into a classified refusal is a change to the
// function name only, never to the message text or its verbs.
func refuse(code wire.ErrorCode, format string, a ...any) error {
	return &Refusal{Code: code, Err: fmt.Errorf(format, a...)}
}
