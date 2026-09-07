package wire

import "testing"

// TestEnvelopeKindLiterals pins each EnvelopeKind constant's string value
// against a literal written in this file. Every other assertion in this
// package compares env.Kind against a Kind* symbol — symbol vs symbol — which
// moves with the declaration and so can never catch the declared value
// itself changing. That value is not internal: the browser dispatches on it
// (see EnvelopeKind's doc comment on types.go), so a hand-typed literal on
// the frontend side is only kept honest if something on this side is pinned
// to the same literal. Each subtest below fails by name, so a broken build
// names exactly which kind's wire value moved.
func TestEnvelopeKindLiterals(t *testing.T) {
	cases := []struct {
		name    string
		kind    EnvelopeKind
		literal string
	}{
		{"KindMessage", KindMessage, "message"},
		{"KindPermission", KindPermission, "permission"},
		{"KindFenced", KindFenced, "fenced"},
		{"KindError", KindError, "error"},
		{"KindResult", KindResult, "result"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if string(c.kind) != c.literal {
				t.Errorf("%s = %q, want %q", c.name, c.kind, c.literal)
			}
		})
	}
}
