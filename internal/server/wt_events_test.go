package server

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/wire"
)

// /wt/events is the one route with no prompt of its own to fail, so every bug it
// can have looks the same from outside: nothing arrives. These tests are about
// the two ends of the loop that decides whether anything does — the envelopes it
// forwards, and the four ways it stops.

// collect drives pumpEvents on its own goroutine and reports what it emitted
// once the loop returns. The channel it returns is closed when the pump exits,
// which is how each test below asserts that a given signal ENDED the stream
// rather than merely being ignored.
func collect(retired <-chan struct{}, done <-chan struct{}, subCh <-chan wire.Envelope, emitErr error) (<-chan []wire.Envelope, chan wire.Envelope) {
	seen := make(chan []wire.Envelope, 1)
	emitted := make(chan wire.Envelope, 16)
	go func() {
		var got []wire.Envelope
		pumpEvents(done, retired, subCh, "watched-sid", "client-a", func(env wire.Envelope) error {
			got = append(got, env)
			emitted <- env
			return emitErr
		})
		seen <- got
	}()
	return seen, emitted
}

// TestPumpEvents_RetirementEndsTheStream is the wiring hop tether#75 turns on.
//
// The registry closing the retirement signal and this loop acting on it are two
// correct halves with a hand-written join between them, and a join is exactly
// where a fix that is right on both sides still ships broken: the daemon would
// have said "this session is over" to a client that goes on waiting, which is
// the same silence one layer out.
func TestPumpEvents_RetirementEndsTheStream(t *testing.T) {
	retired := make(chan struct{})
	subCh := make(chan wire.Envelope)
	seen, _ := collect(retired, nil, subCh, nil)

	select {
	case got := <-seen:
		t.Fatalf("the pump returned before anything happened (emitted %d envelopes)", len(got))
	case <-time.After(50 * time.Millisecond):
	}

	close(retired)
	select {
	case <-seen:
	case <-time.After(time.Second):
		t.Fatal("the observed sid was retired and the read loop kept waiting: the client is " +
			"left on a transport that will never carry another event")
	}
}

// TestPumpEvents_StampsTheWatchedSidOntoAnUnlabelledEnvelope — the observer
// asked about one sid and needs to be told which session it is hearing.
// Registry.broadcast builds nearly every envelope without a SessionID (fanOut
// knows the sid but does not put it on the wire), so without this stamp the
// route's whole output is unattributed. Only the unlabelled ones — see the test
// below for the half that says why.
func TestPumpEvents_StampsTheWatchedSidOntoAnUnlabelledEnvelope(t *testing.T) {
	subCh := make(chan wire.Envelope, 2)
	done := make(chan struct{})
	_, emitted := collect(nil, done, subCh, nil)
	defer close(done)

	subCh <- wire.Envelope{Kind: wire.KindMessage, Payload: "hello"}
	select {
	case env := <-emitted:
		if env.SessionID != "watched-sid" {
			t.Errorf("SessionID = %q, want %q", env.SessionID, "watched-sid")
		}
		if got, _ := env.Payload.(string); got != "hello" {
			t.Errorf("payload = %q, want %q", got, "hello")
		}
	case <-time.After(time.Second):
		t.Fatal("nothing was emitted")
	}
}

// TestPumpEvents_LeavesAnEnvelopeThatNamesItsOwnSession — the other half of the
// stamp, and the reason it is conditional.
//
// Registry.BroadcastAll is daemon-wide: its callers are a permission request for
// a named session (server/mux.go) and the shell lock events (server/wt_shell.go),
// and two of the three set Envelope.SessionID themselves. Stamping the watched
// sid over that told an observer of X that another session's tool-permission
// prompt belonged to X. The overwrite was unconditional before tether#75 and
// reached only observers of live sids; sid-keyed subscription widened the
// audience to observers of sids with no registration at all, which would have
// widened the mislabelling with it.
func TestPumpEvents_LeavesAnEnvelopeThatNamesItsOwnSession(t *testing.T) {
	subCh := make(chan wire.Envelope, 2)
	done := make(chan struct{})
	_, emitted := collect(nil, done, subCh, nil)
	defer close(done)

	subCh <- wire.Envelope{Kind: wire.KindPermission, SessionID: "some-other-session", Payload: "may I"}
	select {
	case env := <-emitted:
		if env.SessionID != "some-other-session" {
			t.Errorf("SessionID = %q, want %q: a daemon-wide notice that names its own session "+
				"must not be relabelled with the sid this connection happens to watch",
				env.SessionID, "some-other-session")
		}
	case <-time.After(time.Second):
		t.Fatal("nothing was emitted")
	}
}

// TestPumpEvents_ClientGoneEndsTheStream — the pre-existing exit, kept honest.
// wtsess.Context() is cancelled when the browser goes away, and a pump that
// ignored it would leak one goroutine per closed tab for the life of the daemon.
func TestPumpEvents_ClientGoneEndsTheStream(t *testing.T) {
	done := make(chan struct{})
	seen, _ := collect(nil, done, make(chan wire.Envelope), nil)

	close(done)
	select {
	case <-seen:
	case <-time.After(time.Second):
		t.Fatal("the client's transport was closed and the read loop kept waiting")
	}
}

// TestPumpEvents_AFailedWriteEndsTheStream — the other pre-existing exit. A
// uni-stream that will not open or will not write means the client is gone;
// carrying on would burn a stream per envelope against a dead transport.
func TestPumpEvents_AFailedWriteEndsTheStream(t *testing.T) {
	subCh := make(chan wire.Envelope, 1)
	seen, _ := collect(nil, nil, subCh, errors.New("stream closed"))

	subCh <- wire.Envelope{Kind: wire.KindMessage, Payload: "will not land"}
	select {
	case got := <-seen:
		if len(got) != 1 {
			t.Errorf("emitted %d envelopes, want 1: the loop must stop at the FIRST failed write", len(got))
		}
	case <-time.After(time.Second):
		t.Fatal("a failed write did not end the read loop")
	}
}

// TestPumpEvents_AClosedSubscriberChannelEndsTheStream — nothing in the daemon
// closes a subscriber channel today (retirement uses its own signal precisely so
// that it does not have to), but the branch is cheap to keep correct and a
// nil-envelope loop spinning on a closed channel would be a busy one.
func TestPumpEvents_AClosedSubscriberChannelEndsTheStream(t *testing.T) {
	subCh := make(chan wire.Envelope)
	seen, _ := collect(nil, nil, subCh, nil)

	close(subCh)
	select {
	case got := <-seen:
		if len(got) != 0 {
			t.Errorf("emitted %d envelopes from a closed channel, want 0", len(got))
		}
	case <-time.After(time.Second):
		t.Fatal("a closed subscriber channel did not end the read loop")
	}
}

// TestServeEvents_PassesTheRetirementSignalToTheReadLoop guards the join the
// test above cannot reach.
//
// pumpEvents proves the loop ACTS on a retirement signal; it says nothing about
// serveEvents handing it the one the registry actually returned. That hop is a
// single hand-written argument, it needs a live WebTransport session to exercise
// at runtime, and dropping it would leave every unit test in this file green
// while the route went back to waiting forever. So it is checked against the
// source, the same way internal/wire asserts its own error table is exhaustive.
//
// Reading the source rather than the binary is deliberate and has a consequence
// worth knowing: `go build -overlay` is invisible to go/parser, so a mutation
// aimed at this test has to edit the file on disk.
func TestServeEvents_PassesTheRetirementSignalToTheReadLoop(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "wt_events.go", nil, 0)
	if err != nil {
		t.Fatalf("parse wt_events.go: %v", err)
	}

	var fn *ast.FuncDecl
	for _, d := range file.Decls {
		if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == "serveEvents" {
			fn = f
			break
		}
	}
	if fn == nil {
		t.Fatal("serveEvents is gone from wt_events.go; this guard needs re-aiming")
	}

	// The identifier serveEvents binds SubscribeObserver's result to, and the
	// argument list it hands pumpEvents. Every argument is recorded, with a
	// placeholder for the ones that are not bare identifiers, so that POSITIONS
	// survive — an argument list collected with the non-identifiers silently
	// dropped renumbers itself the moment one is added or removed, which is a
	// guard that reads the wrong slot and says nothing.
	var subscribeResult string
	var pumpArgs []string
	ast.Inspect(fn, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			for _, rhs := range node.Rhs {
				call, ok := rhs.(*ast.CallExpr)
				if !ok {
					continue
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "SubscribeObserver" {
					continue
				}
				if id, ok := node.Lhs[0].(*ast.Ident); ok {
					subscribeResult = id.Name
				}
			}
		case *ast.CallExpr:
			if id, ok := node.Fun.(*ast.Ident); ok && id.Name == "pumpEvents" {
				for _, arg := range node.Args {
					if a, ok := arg.(*ast.Ident); ok {
						pumpArgs = append(pumpArgs, a.Name)
						continue
					}
					pumpArgs = append(pumpArgs, "<expr>")
				}
			}
		}
		return true
	})

	if subscribeResult == "" {
		t.Fatal("serveEvents no longer binds the result of reg.SubscribeObserver: an observer " +
			"that is never told its sid was retired waits forever (tether#75)")
	}
	// POSITION, not membership. A first draft of this guard asked only whether the
	// identifier appeared anywhere in the argument list, and a mutant that swapped
	// `retired` with the client-disconnect channel compiled, passed, and left the
	// two exits handling each other's case — which is exactly the class of defect
	// a guard on a hand-written join exists to catch.
	const retiredArg = 1
	if len(pumpArgs) <= retiredArg {
		t.Fatalf("pumpEvents is called with %d identifier arguments (%v); this guard reads the "+
			"one at index %d and needs re-aiming", len(pumpArgs), pumpArgs, retiredArg)
	}
	if pumpArgs[retiredArg] != subscribeResult {
		t.Fatalf("serveEvents binds SubscribeObserver's retirement signal to %q but passes %q as "+
			"pumpEvents' `retired` argument (all: %v) — the loop cannot act on a signal it does "+
			"not receive",
			subscribeResult, pumpArgs[retiredArg], pumpArgs)
	}
}
