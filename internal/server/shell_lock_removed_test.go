package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tether#121 D1 — the shell's 60-second input lock is gone, and these are the
// assertions that say so rather than the reasoning.
//
// # Why the load-bearing test here is structural
//
// The bug was a deadline: a time.AfterFunc(60s) armed once when the shell
// connected, never renewed by input (keystrokes never touched the lock), whose
// firing closed a channel the shell's select watched and killed the PTY. Its
// absence CANNOT be proved behaviourally in bounded time — a test that watches an
// idle shell survive for 200ms says nothing about 60s, and a test that waits 60s
// buys certainty at a price no suite should pay, once, let alone under -race.
//
// What can be proved is that the mechanism is not there: the file that held the
// timer is deleted, no identifier it was made of survives anywhere in the daemon,
// and the one function that now decides a shell's lifetime has exactly two exits
// and no clock at all. That is a two-valued fact about the tree, checked with the
// same AST discipline internal/session/activity_test.go uses on its own source —
// including a guard on the guard, because a matcher that has stopped matching
// passes against anything.
//
// The behavioural companion is TestPumpShell_ClientDisconnectTearsEverythingDown
// in wt_shell_leak_test.go: with a live PTY and a live client, pumpShell has no
// exit to take. It is the weaker statement, and it is labelled as such there.

// TestShellLockForceRouteIsGone — the force-takeover endpoint, and above all the
// PREFIX pattern it lived under.
//
// Against the real buildMux, for the reason activityRouteMux's doc gives: a
// routing test that re-declares the patterns it is checking stays green when the
// registration is deleted, which is the one thing it exists to catch. Here the
// registration IS the deletion, so the same hazard runs the other way — a
// hand-rolled mux would report a removal that had not happened.
//
// Every row is a real before/after. `handleLockForce` answered a GET with 405
// ("method not allowed") on its own method check, so on the old build the first
// two rows were 405 and not 501: the status distinguishes "the handler is still
// wired" from "nothing claims this path".
func TestShellLockForceRouteIsGone(t *testing.T) {
	mux, req := activityRouteMux(t, nil)

	for _, tc := range []struct {
		path     string
		wantCode int
		wantBody string
		why      string
	}{
		{
			"/api/v1/session/abc/lock/force", http.StatusNotImplemented, "not implemented",
			"the endpoint itself: unregistered, so it falls through to the /api/v1/ stub. 405 here would mean handleLockForce is still wired",
		},
		{
			"/api/v1/session/", http.StatusNotImplemented, "not implemented",
			"the singular PREFIX is gone too, not merely its one leaf — it never served anything else",
		},
		{
			"/api/v1/session-activity", http.StatusOK, "{}",
			"the neighbour that removed prefix was cited as a hazard for (one hyphen away) is still served, so the removal did not take a real route with it",
		},
		{
			"/api/v1/sessions", http.StatusOK, "[]",
			"and the plural family is untouched",
		},
		{
			"/api/v1/sessions/633e5ed8-cada-422a-aee1-c7a3502eb4fd/messages", http.StatusOK, "[]",
			"including the transcript route under it",
		},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req(tc.path))
			if rec.Code != tc.wantCode {
				t.Fatalf("GET %s -> %d, want %d (%s); body: %s",
					tc.path, rec.Code, tc.wantCode, tc.why, strings.TrimSpace(rec.Body.String()))
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("GET %s body = %q, want it to contain %q (%s)",
					tc.path, strings.TrimSpace(rec.Body.String()), tc.wantBody, tc.why)
			}
		})
	}
}

// shellLockSymbols are the identifiers the guillotine was built out of. Their
// absence is the checkable form of "the lock is gone".
//
// Matched as IDENTIFIERS, never as text. That is the whole reason this test parses
// instead of grepping, and it cuts both ways: a doc comment or a string literal
// naming one of these — this file does both — must not trip the guard, while a
// declaration or a call must. internal/session/activity_test.go's mirror guard
// makes the opposite demand of its matcher for the same underlying reason.
var shellLockSymbols = []string{
	"SessionLock",     // the type
	"ForceAcquire",    // the takeover that no UI could reach
	"lockTimeout",     // the 60 seconds
	"GetLock",         // Registry's lazy factory for it
	"handleLockForce", // the HTTP endpoint
}

// TestShellLockSymbolsAreGoneFromTheDaemon walks every Go file the daemon is
// built from and fails on any of shellLockSymbols appearing as an identifier.
func TestShellLockSymbolsAreGoneFromTheDaemon(t *testing.T) {
	// The timer's own file. Checked separately because "no identifiers" would also
	// be satisfied by a lock.go that had been emptied rather than removed.
	if _, err := os.Stat(filepath.Join("..", "session", "lock.go")); err == nil {
		t.Error("internal/session/lock.go is back: that file WAS the 60-second guillotine")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat internal/session/lock.go: %v", err)
	}

	files := daemonGoFiles(t)
	// Vacuity guard. A walk that silently stopped finding files would pass against
	// anything, and this suite has a neighbour that says so out loud
	// (activity_test.go: "found zero ... so it would pass against anything").
	if len(files) < 60 {
		t.Fatalf("walked %d Go files under internal/ and cmd/; that is too few to be the daemon, so the collecting half of this guard is broken", len(files))
	}

	blocked := make(map[string]bool, len(shellLockSymbols))
	for _, s := range shellLockSymbols {
		blocked[s] = true
	}

	fset := token.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || !blocked[id.Name] {
				return true
			}
			t.Errorf("%s: %q is back as an identifier.\n"+
				"tether#121 removed the shell input lock because it killed a shell 60 seconds\n"+
				"after it opened whether or not anyone was typing. Re-adding any part of it needs\n"+
				"a test that a live shell survives the deadline, which is why this guard is here\n"+
				"and not a lint rule.", fset.Position(id.Pos()), id.Name)
			return true
		})
	}
}

// TestShellLifetimeHasNoClock — pumpShell is the only place a shell's lifetime is
// decided, and it must have exactly two exits and no clock.
//
// Two exits, not three: the third was `case <-preempted:`, fed by the lock's
// timer. No clock: a deadline needs one, so a function with no reference to `time`
// at all cannot host a replacement — including inside the two pump goroutines,
// which ast.Inspect walks because they are part of the function body.
func TestShellLifetimeHasNoClock(t *testing.T) {
	src, err := os.ReadFile("wt_shell.go")
	if err != nil {
		t.Fatalf("read wt_shell.go: %v", err)
	}
	shape := funcShapeOf(t, "wt_shell.go", src, "pumpShell")

	if !shape.found {
		t.Fatal("no func pumpShell in wt_shell.go — this guard is checking a function that is not there, so it proves nothing")
	}
	if shape.selects != 1 {
		t.Errorf("pumpShell contains %d select statements, want exactly 1: a shell's lifetime must be decided in one place", shape.selects)
	}
	if shape.commClauses != 2 {
		t.Errorf("pumpShell's select has %d cases, want exactly 2 (the PTY exited; the client went away).\n"+
			"A third arm is how the 60-second guillotine reached the shell: the lock closed a\n"+
			"`preempted` channel and this select killed the PTY on it.", shape.commClauses)
	}
	if shape.usesTime {
		t.Error("pumpShell references the time package.\n" +
			"A shell is ended by its process exiting or its client leaving, and by nothing on a\n" +
			"clock — that was tether#121. A legitimate clock here (a write deadline, say) has to\n" +
			"arrive with a test proving it cannot terminate a live shell, and with this guard\n" +
			"narrowed rather than deleted.")
	}
}

// TestShellLifetimeGuardRejectsTheOldShape is the guard on the guard.
//
// funcShapeOf is asserted against the shape it exists to reject — the pre-tether#121
// select, reconstructed — and against a name that is not in the source at all. A
// matcher that had stopped matching would pass TestShellLifetimeHasNoClock while
// the guillotine was back, and that is strictly worse than having no test.
func TestShellLifetimeGuardRejectsTheOldShape(t *testing.T) {
	const guillotine = `package server

func pumpShell(ctx context.Context) {
	lock.timer = time.AfterFunc(lockTimeout, func() { close(preempted) })
	select {
	case <-done:
	case <-preempted:
		closePTY()
	case <-ctx.Done():
		closePTY()
	}
}
`
	old := funcShapeOf(t, "guillotine.go", []byte(guillotine), "pumpShell")
	if !old.found {
		t.Fatal("funcShapeOf did not even find the function in the sample: it cannot be trusted on the real file")
	}
	if old.commClauses != 3 {
		t.Errorf("funcShapeOf counted %d cases in the old three-armed select, want 3", old.commClauses)
	}
	if !old.usesTime {
		t.Error("funcShapeOf did not notice time.AfterFunc — the clock check does not work, so its silence on the real file means nothing")
	}

	absent := funcShapeOf(t, "guillotine.go", []byte(guillotine), "notAFunctionInThere")
	if absent.found {
		t.Error("funcShapeOf reported finding a function that is not in the source: `found` is not measuring anything")
	}
}

// funcShape is what the two tests above compare.
type funcShape struct {
	found       bool
	selects     int
	commClauses int
	usesTime    bool
}

func funcShapeOf(t *testing.T, filename string, src []byte, funcName string) funcShape {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	var out funcShape
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != funcName || fd.Body == nil {
			continue
		}
		out.found = true
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.SelectStmt:
				out.selects++
				out.commClauses += len(v.Body.List)
			case *ast.SelectorExpr:
				if id, ok := v.X.(*ast.Ident); ok && id.Name == "time" {
					out.usesTime = true
				}
			}
			return true
		})
	}
	return out
}

// daemonGoFiles lists every Go file the tether daemon is built from: internal/
// and cmd/, tests included.
//
// v0/ (the archived v1 tree, which has a session lock of its own) and poc/ are
// outside both roots and therefore outside this guard, which is deliberate — they
// are not built into the daemon and CLAUDE.md says v0/ is not to be modified.
func daemonGoFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, sub := range []string{"internal", "cmd"} {
		dir := filepath.Join("..", "..", sub)
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(path, ".go") {
				out = append(out, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	return out
}
