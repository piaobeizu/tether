package wire

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// TestTerminalCodesExhaustive is an anti-rot guard: it parses this package's
// OWN source with go/ast, collects the name of every declared const whose
// type is ErrorCode, and asserts each one is a key in terminalCodes. Without
// this, adding a ninth ErrorCode and forgetting to classify it would compile
// fine and silently default through Terminal() to "retryable" — exactly the
// kind of gap the package doc comment says the exhaustiveness test exists to
// close. Parsing the source (rather than, say, hand-maintaining a parallel
// list in this file) means the guard cannot itself go stale: it always sees
// whatever consts errors.go currently declares.
func TestTerminalCodesExhaustive(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse wire package source: %v", err)
	}

	var codes []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					continue
				}
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					// Two ways to be in scope, because requiring the explicit
					// type left a hole a review found by mutation: an UNTYPED
					// `ErrCodeSomething = "..."` is still usable everywhere an
					// ErrorCode is wanted (untyped string consts convert
					// implicitly at the call), so it would have shipped
					// unclassified while this guard stayed green. Name prefix
					// catches that; the type check still catches a code named
					// something else entirely.
					typed := false
					if vs.Type != nil {
						if ident, ok := vs.Type.(*ast.Ident); ok && ident.Name == "ErrorCode" {
							typed = true
						}
					}
					named := false
					for _, n := range vs.Names {
						if strings.HasPrefix(n.Name, "ErrCode") {
							named = true
						}
					}
					if !typed && !named {
						continue
					}
					for i, name := range vs.Names {
						if i >= len(vs.Values) {
							t.Fatalf("ErrorCode const %s has no explicit value; this test assumes every ErrorCode const is declared name ErrorCode = \"literal\"", name.Name)
						}
						lit, ok := vs.Values[i].(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							t.Fatalf("ErrorCode const %s is not a string literal; cannot verify its classification statically", name.Name)
						}
						val, err := strconv.Unquote(lit.Value)
						if err != nil {
							t.Fatalf("unquote %s's value %s: %v", name.Name, lit.Value, err)
						}
						codes = append(codes, val)
					}
				}
			}
		}
	}

	// A zero-code result means the parser found nothing to check, which is a
	// broken guard, not a passing one — fail loudly rather than vacuously
	// succeeding.
	if len(codes) == 0 {
		t.Fatal("found zero ErrorCode consts while parsing the wire package — the exhaustiveness guard is not seeing errors.go")
	}

	for _, code := range codes {
		if _, ok := terminalCodes[ErrorCode(code)]; !ok {
			t.Errorf("ErrorCode %q is declared but has no entry in terminalCodes — classify it as terminal or retryable", code)
		}
	}
}

// TestErrorCodeDisposition pins the terminal/retryable call for every code
// this package declares. It is a table, not a loop over terminalCodes itself,
// because the point is to catch an ACCIDENTAL flip of one entry (e.g. tether#63
// M1: ErrCodeUnknownWorkspace flipped to false) — asserting against the map
// that IS the classification would not catch the map itself being wrong.
func TestErrorCodeDisposition(t *testing.T) {
	cases := []struct {
		code     ErrorCode
		terminal bool
	}{
		{ErrCodeUnknownWorkspace, true},
		{ErrCodeNoWorkspaceRegistry, true},
		{ErrCodeUnknownProvider, true},
		{ErrCodeSessionOwned, true},
		{ErrCodeSpawnFailed, false},
		{ErrCodeConnectionClosed, false},
		{ErrCodeSessionUnconfirmed, false},
		{ErrCodeAgent, false},
	}
	for _, c := range cases {
		if got := c.code.Terminal(); got != c.terminal {
			t.Errorf("%s.Terminal() = %v, want %v", c.code, got, c.terminal)
		}
	}
}

// TestUnknownWorkspaceVsNoRegistryDiffer pins the hard requirement from
// resolveWorkspace's doc comment: an unknown workspace id and a daemon with no
// registry at all are DIFFERENT codes, because they are different bugs an
// operator would chase differently. This is the wire-level half of that
// guarantee; internal/session has the behavioural half (that resolveWorkspace
// actually emits these two codes on the two respective paths).
func TestUnknownWorkspaceVsNoRegistryDiffer(t *testing.T) {
	if ErrCodeUnknownWorkspace == ErrCodeNoWorkspaceRegistry {
		t.Fatal("ErrCodeUnknownWorkspace and ErrCodeNoWorkspaceRegistry must be distinct codes")
	}
}

// TestErrorCodeTerminalUnknownDefaultsFalse pins the deliberate default for a
// code this build does not recognize (e.g. an older frontend after a partial
// deploy introduces a new code): retryable, not terminal. See the package doc
// comment's "why an unclassified code defaults to retryable" for the
// reasoning.
func TestErrorCodeTerminalUnknownDefaultsFalse(t *testing.T) {
	if got := ErrorCode("some_future_code_this_build_has_never_seen").Terminal(); got != false {
		t.Fatalf("unclassified ErrorCode.Terminal() = %v, want false (must not brick a healthy client)", got)
	}
}

// TestNewErrorEnvelopeShape pins the JSON wire shape NewErrorEnvelope
// produces, since it is the only constructor this package exposes for a
// KindError envelope and tygo mirrors this shape into wire.gen.ts.
func TestNewErrorEnvelopeShape(t *testing.T) {
	env := NewErrorEnvelope(ErrCodeUnknownWorkspace, "unknown workspace \"foo\"")
	if env.Kind != KindError {
		t.Fatalf("Kind = %q, want %q", env.Kind, KindError)
	}
	payload, ok := env.Payload.(ErrorPayload)
	if !ok {
		t.Fatalf("Payload is %T, want ErrorPayload", env.Payload)
	}
	if payload.Code != ErrCodeUnknownWorkspace {
		t.Errorf("Code = %q, want %q", payload.Code, ErrCodeUnknownWorkspace)
	}
	if payload.Message != `unknown workspace "foo"` {
		t.Errorf("Message = %q, want the verbatim message passed in", payload.Message)
	}
	if !payload.Terminal {
		t.Errorf("Terminal = false, want true for %q", ErrCodeUnknownWorkspace)
	}

	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	payloadRaw, ok := raw["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload key is %T, want an object", raw["payload"])
	}
	for _, key := range []string{"code", "message", "terminal"} {
		if _, ok := payloadRaw[key]; !ok {
			t.Errorf("marshaled payload is missing key %q: %v", key, payloadRaw)
		}
	}
	if payloadRaw["code"] != string(ErrCodeUnknownWorkspace) {
		t.Errorf("payload.code = %v, want %q", payloadRaw["code"], ErrCodeUnknownWorkspace)
	}
	if payloadRaw["terminal"] != true {
		t.Errorf("payload.terminal = %v, want true", payloadRaw["terminal"])
	}
}

// TestNewErrorEnvelopeRetryableCode pins the retryable side of the same
// constructor, so a regression that always set Terminal:true (defeating the
// entire point of shipping the bit) would be caught here rather than only in
// the terminal-side test above.
func TestNewErrorEnvelopeRetryableCode(t *testing.T) {
	env := NewErrorEnvelope(ErrCodeSpawnFailed, "spawn: exec: \"cc\": executable file not found in $PATH")
	payload, ok := env.Payload.(ErrorPayload)
	if !ok {
		t.Fatalf("Payload is %T, want ErrorPayload", env.Payload)
	}
	if payload.Terminal {
		t.Errorf("Terminal = true, want false for %q", ErrCodeSpawnFailed)
	}
}
