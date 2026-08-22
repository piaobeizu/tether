package server

import (
	"net/url"
	"os"
	"os/exec"
	"testing"

	"github.com/creack/pty"

	"github.com/piaobeizu/tether/internal/session"
	"github.com/piaobeizu/tether/internal/wire"
)

// ─── the wiring hop (tether#68) ─────────────────────────────────────────────
//
// The handlers below are individually trivial; what silently does nothing when
// it is missing is the hop BETWEEN them — a "resize" frame reaching the shell's
// PTY at all. These tests drive routeClientFrame (the real dispatch serveControl
// uses) and assert on the VALUES that arrive at the registered hook, so a hook
// that fires with the wrong numbers fails just as loudly as one that never fires.

// recordResize registers a shell-resize hook for sid under shellID and returns a
// pointer to the last size it observed. cols/rows are -1 until the hook runs.
func recordResize(reg *session.Registry, sid, shellID string) *[2]int {
	got := &[2]int{-1, -1}
	reg.RegisterShellResize(sid, shellID, func(cols, rows uint16) error {
		got[0], got[1] = int(cols), int(rows)
		return nil
	})
	return got
}

func TestRouteClientFrame_ResizeReachesTheShellHook(t *testing.T) {
	reg := session.NewRegistry()
	got := recordResize(reg, "sid-1", "shell-1")

	resp, ok := routeClientFrame(reg, wire.ClientFrame{
		Kind:      wire.ClientFrameResize,
		SessionID: "sid-1",
		ShellID:   "shell-1",
		Cols:      143,
		Rows:      41,
	})

	if ok || resp != nil {
		t.Fatalf("resize frame should not produce a reply, got resp=%v ok=%v", resp, ok)
	}
	// The values must come from the frame's same-named fields — not from a
	// default, not transposed. 143 != 41 so a Cols/Rows swap is caught too.
	if got[0] != 143 || got[1] != 41 {
		t.Fatalf("hook got cols=%d rows=%d, want cols=143 rows=41", got[0], got[1])
	}
}

// A resize frame must reach the hook registered for ITS session, and no other.
func TestRouteClientFrame_ResizeRoutesBySessionID(t *testing.T) {
	reg := session.NewRegistry()
	target := recordResize(reg, "sid-target", "shell-target")
	other := recordResize(reg, "sid-other", "shell-other")

	routeClientFrame(reg, wire.ClientFrame{
		Kind: wire.ClientFrameResize, SessionID: "sid-target", ShellID: "shell-target", Cols: 100, Rows: 30,
	})

	if target[0] != 100 || target[1] != 30 {
		t.Fatalf("target hook got %v, want [100 30]", *target)
	}
	if other[0] != -1 || other[1] != -1 {
		t.Fatalf("other session's hook must not fire, got %v", *other)
	}
}

// The empty sid is a real case: ShellPane connects before any chat session
// exists, so the shell is registered under "". Resize must still route.
func TestRouteClientFrame_ResizeRoutesForEmptySessionID(t *testing.T) {
	reg := session.NewRegistry()
	got := recordResize(reg, "", "shell-preSession")

	routeClientFrame(reg, wire.ClientFrame{
		Kind: wire.ClientFrameResize, SessionID: "", ShellID: "shell-preSession", Cols: 80, Rows: 24,
	})

	if got[0] != 80 || got[1] != 24 {
		t.Fatalf("hook got %v, want [80 24]", *got)
	}
}

// A zero dimension would blank the remote TUI; drop the frame instead.
func TestRouteClientFrame_ResizeIgnoresZeroDimension(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cols, rows uint16
	}{
		{"zero cols", 0, 24},
		{"zero rows", 80, 0},
		{"both zero", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := session.NewRegistry()
			got := recordResize(reg, "sid-1", "shell-1")
			routeClientFrame(reg, wire.ClientFrame{
				Kind: wire.ClientFrameResize, SessionID: "sid-1", ShellID: "shell-1", Cols: tc.cols, Rows: tc.rows,
			})
			if got[0] != -1 || got[1] != -1 {
				t.Fatalf("hook must not fire for %s, got %v", tc.name, *got)
			}
		})
	}
}

// /wt/control is not session-scoped, so a resize can arrive after the shell
// closed. That must be a dropped frame, not a panic.
func TestRouteClientFrame_ResizeForUnknownSessionDoesNotPanic(t *testing.T) {
	reg := session.NewRegistry()
	routeClientFrame(reg, wire.ClientFrame{
		Kind: wire.ClientFrameResize, SessionID: "never-registered", Cols: 80, Rows: 24,
	})
}

// Guard the dispatch itself: the pre-existing kinds must keep their behaviour
// now that they share a switch with resize.
func TestRouteClientFrame_PingStillAnswered(t *testing.T) {
	reg := session.NewRegistry()
	resp, ok := routeClientFrame(reg, wire.ClientFrame{Kind: wire.ClientFramePing, TS: 99})
	if !ok || resp == nil || resp.Kind != wire.ControlPong || resp.TS != 99 {
		t.Fatalf("ping must still be answered with a pong echoing TS, got resp=%+v ok=%v", resp, ok)
	}
}

// ─── initial size on the /wt/shell query string ─────────────────────────────

func TestParseWinsize(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  *pty.Winsize
	}{
		{"valid", "cols=120&rows=40", &pty.Winsize{Cols: 120, Rows: 40}},
		{"missing both", "", nil},
		{"missing rows", "cols=120", nil},
		{"missing cols", "rows=40", nil},
		{"non-numeric", "cols=wide&rows=40", nil},
		{"zero cols", "cols=0&rows=40", nil},
		{"zero rows", "cols=120&rows=0", nil},
		{"negative", "cols=-1&rows=40", nil},
		{"overflows uint16", "cols=70000&rows=40", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatalf("bad test query: %v", err)
			}
			got := parseWinsize(q)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("want nil (fall back to kernel default), got %+v", got)
			case tc.want != nil && got == nil:
				t.Fatalf("want %+v, got nil", tc.want)
			case tc.want != nil && (got.Cols != tc.want.Cols || got.Rows != tc.want.Rows):
				t.Fatalf("got cols=%d rows=%d, want cols=%d rows=%d",
					got.Cols, got.Rows, tc.want.Cols, tc.want.Rows)
			}
		})
	}
}

// parseShellRequest is where the /wt/shell query keys are named, and the only
// place they are checkable: handleWTShell needs a live WebTransport session, so
// nothing reaches the reads if they stay inline there.
//
// The shell id is the case that has to be here rather than left to review. A
// wrong or missing sid shows up as "No conversation found" and a wrong size
// shows up on screen, but a shell id that never arrives is silent — it just puts
// the daemon back on the pre-tether#150 fallback.
func TestParseShellRequest(t *testing.T) {
	// Built with the SAME constant the SPA writes the URL with, so a rename that
	// touched only one side of the contract fails here.
	q := url.Values{}
	q.Set("sid", "sid-42")
	q.Set(wire.ShellQueryParam, "sh-abc123")
	q.Set("cols", "120")
	q.Set("rows", "40")

	got := parseShellRequest(q)
	if got.sid != "sid-42" {
		t.Fatalf("sid = %q, want %q", got.sid, "sid-42")
	}
	if got.shellID != "sh-abc123" {
		t.Fatalf("shellID = %q, want %q", got.shellID, "sh-abc123")
	}
	if got.winsize == nil || got.winsize.Cols != 120 || got.winsize.Rows != 40 {
		t.Fatalf("winsize = %+v, want cols=120 rows=40", got.winsize)
	}

	// A client that names nothing must still get a shell, on the empty id — that
	// is what keeps Registry.ResizeShell's fallback reachable rather than dead.
	bare := parseShellRequest(url.Values{})
	if bare.sid != "" || bare.shellID != "" || bare.winsize != nil {
		t.Fatalf("empty query gave %+v, want a zero shellRequest", bare)
	}
}

// The whole path a real connection takes: query string → registration →
// /wt/control frame → the kernel's winsize, with a SECOND shell on the same
// session standing next to it the entire time.
//
// This exists because the two ends meet at the shell id, and a build where the
// query key and the frame's ShellID disagreed would satisfy each end's own tests
// and still route nothing. Measured against the mutants: a build that ignores
// ShellID when routing fails here, and so does one whose query key drifted from
// wire.ShellQueryParam — but a build that releases registrations by sid does NOT,
// which is why the "after the other pane exits" cases below are separate.
func TestShellResize_QueryStringNamesTheShellAFrameThenFinds(t *testing.T) {
	reg := session.NewRegistry()

	q, err := url.ParseQuery("sid=sid-two-panes&" + wire.ShellQueryParam + "=sh-mine&cols=80&rows=24")
	if err != nil {
		t.Fatalf("bad test query: %v", err)
	}
	req := parseShellRequest(q)

	cmd := exec.Command("sleep", "30")
	ptmx, err := startPTY(cmd, req.winsize)
	if err != nil {
		t.Fatalf("startPTY: %v", err)
	}
	defer func() { _ = ptmx.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	defer attachShellResize(reg, req.sid, req.shellID, ptmx)()

	// The neighbour. Its presence is the point: with one shell on the session the
	// fallback would answer this frame no matter what the ids said.
	other := openLiveShell(t, reg, req.sid, "sh-theirs", 80, 24)

	resizeFrame(reg, req.sid, "sh-mine", 176, 48)

	ws, err := pty.GetsizeFull(ptmx)
	if err != nil {
		t.Fatalf("GetsizeFull: %v", err)
	}
	if ws.Cols != 176 || ws.Rows != 48 {
		t.Fatalf("shell named on the URL is cols=%d rows=%d, want cols=176 rows=48", ws.Cols, ws.Rows)
	}
	other.mustBe(t, 80, 24)
}

// startPTY must actually apply the size — asserted by reading it back off the
// PTY rather than by trusting that the sized branch was taken.
func TestStartPTY_AppliesRequestedSize(t *testing.T) {
	cmd := exec.Command("sleep", "10")
	ptmx, err := startPTY(cmd, &pty.Winsize{Cols: 133, Rows: 37})
	if err != nil {
		t.Fatalf("startPTY: %v", err)
	}
	defer func() { _ = ptmx.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	ws, err := pty.GetsizeFull(ptmx)
	if err != nil {
		t.Fatalf("GetsizeFull: %v", err)
	}
	if ws.Cols != 133 || ws.Rows != 37 {
		t.Fatalf("PTY started at cols=%d rows=%d, want cols=133 rows=37", ws.Cols, ws.Rows)
	}
}

// A nil size must degrade to the pre-tether#68 behaviour (kernel default),
// not fail the shell.
func TestStartPTY_NilSizeStillStarts(t *testing.T) {
	cmd := exec.Command("sleep", "10")
	ptmx, err := startPTY(cmd, nil)
	if err != nil {
		t.Fatalf("startPTY(nil): %v", err)
	}
	_ = ptmx.Close()
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// ─── frame → kernel, end to end ─────────────────────────────────────────────

// The one test that fails if ANY link in the chain is removed: attachShellResize
// not called, the resize case missing from routeClientFrame, ResizeShell not
// looking the hook up, or Setsize given transposed dimensions. It asserts on the
// kernel's winsize, so nothing short of the real thing satisfies it.
func TestShellResize_FrameReachesTheKernel(t *testing.T) {
	reg := session.NewRegistry()
	cmd := exec.Command("sleep", "10")
	ptmx, err := startPTY(cmd, &pty.Winsize{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("startPTY: %v", err)
	}
	defer func() { _ = ptmx.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	detach := attachShellResize(reg, "sid-e2e", "shell-e2e", ptmx)

	routeClientFrame(reg, wire.ClientFrame{
		Kind: wire.ClientFrameResize, SessionID: "sid-e2e", ShellID: "shell-e2e", Cols: 176, Rows: 48,
	})

	ws, err := pty.GetsizeFull(ptmx)
	if err != nil {
		t.Fatalf("GetsizeFull: %v", err)
	}
	if ws.Cols != 176 || ws.Rows != 48 {
		t.Fatalf("PTY is cols=%d rows=%d after resize frame, want cols=176 rows=48", ws.Cols, ws.Rows)
	}

	// After detach the PTY must stop tracking further frames — otherwise a
	// closed shell's fd keeps being poked by a stale session id.
	detach()
	routeClientFrame(reg, wire.ClientFrame{
		Kind: wire.ClientFrameResize, SessionID: "sid-e2e", ShellID: "shell-e2e", Cols: 100, Rows: 30,
	})
	ws, err = pty.GetsizeFull(ptmx)
	if err != nil {
		t.Fatalf("GetsizeFull after detach: %v", err)
	}
	if ws.Cols != 176 || ws.Rows != 48 {
		t.Fatalf("detached shell still resized to cols=%d rows=%d", ws.Cols, ws.Rows)
	}
}

// ─── two live shells on ONE session (tether#150) ─────────────────────────────
//
// The premise of every case in this section is a state that could not happen
// before tether#121: one sid with TWO live /wt/shell connections, each on its
// own PTY. Two tabs, or a phone and a laptop. Nothing here is timing-sensitive
// and nothing races — a "shell" is created and released by an explicit call, so
// the order of the two opens and the two closes is written down rather than
// scheduled.
//
// What the defect looked like, as numbers, since that is what makes these
// assertions gates rather than decoration. With the resize map keyed by sid:
//
//   - a frame naming the FIRST pane was applied to the SECOND pane's PTY, so
//     the first pane stayed at the size it was started with (80x24 below) while
//     the second moved to a size nobody had asked it for;
//   - the second pane's exit deleted the sid's only entry, so from then on every
//     frame for the first pane was dropped and its PTY stayed at 80x24 for the
//     rest of its life — the half of this bug a user cannot work around by
//     dragging the divider again.

// liveShell is one /wt/shell connection's worth of state: a real PTY with a real
// process on it, registered exactly the way handleWTShell registers one.
type liveShell struct {
	shellID string
	ptmx    *os.File
	release func()
}

// openLiveShell starts a PTY at cols x rows and attaches it, as (sid, shellID).
// It verifies the start size by reading it back off the kernel, so a case that
// later asserts "still 80x24" cannot be passing because the PTY was never sized
// in the first place.
func openLiveShell(t *testing.T, reg *session.Registry, sid, shellID string, cols, rows uint16) *liveShell {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	ptmx, err := startPTY(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		t.Fatalf("startPTY for %q: %v", shellID, err)
	}
	sh := &liveShell{shellID: shellID, ptmx: ptmx}
	sh.release = attachShellResize(reg, sid, shellID, ptmx)
	t.Cleanup(func() {
		sh.release()
		_ = ptmx.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	sh.mustBe(t, cols, rows)
	return sh
}

// mustBe asserts the KERNEL's idea of this PTY's size. Not the registry's, not
// the hook's — a resize that got as far as a recorded callback and no further is
// the failure this whole file exists to catch.
func (s *liveShell) mustBe(t *testing.T, cols, rows uint16) {
	t.Helper()
	ws, err := pty.GetsizeFull(s.ptmx)
	if err != nil {
		t.Fatalf("GetsizeFull(%s): %v", s.shellID, err)
	}
	if ws.Cols != cols || ws.Rows != rows {
		t.Fatalf("shell %s is cols=%d rows=%d, want cols=%d rows=%d",
			s.shellID, ws.Cols, ws.Rows, cols, rows)
	}
}

// resizeFrame drives the real /wt/control dispatch, so these cases cover the
// frame's ShellID being carried through routeClientFrame and handleResizeFrame
// as well as the routing itself.
func resizeFrame(reg *session.Registry, sid, shellID string, cols, rows uint16) {
	routeClientFrame(reg, wire.ClientFrame{
		Kind: wire.ClientFrameResize, SessionID: sid, ShellID: shellID, Cols: cols, Rows: rows,
	})
}

// FAILURE ONE: the older pane's resize reaching the newer pane's PTY.
//
// Both shells are live for the whole case. Each assertion names the shell it is
// about, and the other shell's size is asserted alongside it — a fix that
// resized BOTH PTYs on every frame would satisfy "the one I asked for moved"
// and is just as wrong.
func TestShellResize_TwoLiveShellsOnOneSessionAreResizedIndependently(t *testing.T) {
	reg := session.NewRegistry()
	const sid = "sid-two-panes"

	older := openLiveShell(t, reg, sid, "shell-older", 80, 24)
	newer := openLiveShell(t, reg, sid, "shell-newer", 80, 24)

	// The older pane, opened FIRST and therefore the one the sid-keyed map had
	// already forgotten. Under the defect this frame went to shell-newer and
	// older stayed at 80x24.
	resizeFrame(reg, sid, older.shellID, 176, 48)
	older.mustBe(t, 176, 48)
	newer.mustBe(t, 80, 24)

	// And the newer pane still resizes itself, without disturbing the older one.
	resizeFrame(reg, sid, newer.shellID, 100, 30)
	newer.mustBe(t, 100, 30)
	older.mustBe(t, 176, 48)
}

// FAILURE TWO: the newer pane's exit taking the older pane's resize with it.
//
// This is a different failure from the one above and needs its own case: a fix
// that routed frames correctly but still released by sid would pass that test
// and fail this one. The exit is an explicit release() call, so "the newer pane
// has finished closing" is a fact here and not a hope about scheduling.
func TestShellResize_OlderShellStillResizesAfterTheNewerOneExits(t *testing.T) {
	reg := session.NewRegistry()
	const sid = "sid-two-panes"

	older := openLiveShell(t, reg, sid, "shell-older", 80, 24)
	newer := openLiveShell(t, reg, sid, "shell-newer", 80, 24)

	// Precondition, asserted rather than assumed: while both are live the older
	// pane can be resized. Without this the case below would also pass against a
	// build where the older pane was never resizable at all.
	resizeFrame(reg, sid, older.shellID, 120, 36)
	older.mustBe(t, 120, 36)

	// The user closes the second tab. Its own PTY goes with it; the first tab's
	// must not.
	newer.release()

	resizeFrame(reg, sid, older.shellID, 140, 44)
	older.mustBe(t, 140, 44)

	// Nothing routes to the departed shell any more, either.
	if err := reg.ResizeShell(sid, newer.shellID, 90, 26); err == nil {
		t.Fatalf("resize for a released shell must fail, got nil error")
	}
	newer.mustBe(t, 80, 24)
}

// The order of the two exits must not matter: the OLDER pane leaving must not
// take the newer pane's resizing with it either. Keyed by sid, this direction
// was broken too — the older pane's deferred unregister deleted the entry the
// newer pane had just installed.
func TestShellResize_NewerShellStillResizesAfterTheOlderOneExits(t *testing.T) {
	reg := session.NewRegistry()
	const sid = "sid-two-panes"

	older := openLiveShell(t, reg, sid, "shell-older", 80, 24)
	newer := openLiveShell(t, reg, sid, "shell-newer", 80, 24)

	older.release()

	resizeFrame(reg, sid, newer.shellID, 150, 45)
	newer.mustBe(t, 150, 45)
	older.mustBe(t, 80, 24)
}

// A frame that names a shell which is not live must be DROPPED, not applied to
// whatever else is on the session.
//
// Without this, "keyed by shell instance" is decoration: a stale frame from a
// pane that closed a moment ago would still land on its neighbour's PTY, which
// is the same wrong-terminal resize tether#150 is about, just arriving by a
// different route.
func TestShellResize_UnknownShellIDDoesNotFallBackOntoAnotherShell(t *testing.T) {
	reg := session.NewRegistry()
	const sid = "sid-two-panes"

	older := openLiveShell(t, reg, sid, "shell-older", 80, 24)
	newer := openLiveShell(t, reg, sid, "shell-newer", 80, 24)

	resizeFrame(reg, sid, "shell-that-never-existed", 200, 60)
	older.mustBe(t, 80, 24)
	newer.mustBe(t, 80, 24)

	if err := reg.ResizeShell(sid, "shell-that-never-existed", 200, 60); err == nil {
		t.Fatalf("resize naming an unknown shell must report it, got nil error")
	}
}

// A shell id is only meaningful inside its session: the same id on two sessions
// is two different terminals, and a frame must not cross between them.
func TestShellResize_ShellIDIsScopedToItsSession(t *testing.T) {
	reg := session.NewRegistry()

	mine := openLiveShell(t, reg, "sid-mine", "shell-1", 80, 24)
	theirs := openLiveShell(t, reg, "sid-theirs", "shell-1", 80, 24)

	resizeFrame(reg, "sid-mine", "shell-1", 111, 33)
	mine.mustBe(t, 111, 33)
	theirs.mustBe(t, 80, 24)
}

// A client that names no shell — every client before tether#150 — keeps the
// routing it had: the session's most recently opened live shell.
//
// The part that is new is the second half. Under the defect the newer pane's
// exit deleted the sid's only entry, so an unnamed frame after it was dropped
// forever; now it falls through to the shell that is still there.
func TestShellResize_UnnamedFrameGoesToTheNewestLiveShell(t *testing.T) {
	reg := session.NewRegistry()
	const sid = "sid-two-panes"

	older := openLiveShell(t, reg, sid, "shell-older", 80, 24)
	newer := openLiveShell(t, reg, sid, "shell-newer", 80, 24)

	resizeFrame(reg, sid, "", 130, 40)
	newer.mustBe(t, 130, 40)
	older.mustBe(t, 80, 24)

	newer.release()

	resizeFrame(reg, sid, "", 160, 50)
	older.mustBe(t, 160, 50)
	newer.mustBe(t, 130, 40)
}

// Releasing twice, or after everything else has gone, must not remove a later
// shell that happens to be on the same session. Ids are never reused, so this is
// a property of the registry rather than of call order — pinned here because
// handleWTShell's `defer release()` can run after a reconnect has already
// installed the next shell.
func TestShellResize_ReleasingTwiceDoesNotDisturbALaterShell(t *testing.T) {
	reg := session.NewRegistry()
	const sid = "sid-reconnect"

	gone := openLiveShell(t, reg, sid, "shell-gone", 80, 24)
	gone.release()

	fresh := openLiveShell(t, reg, sid, "shell-fresh", 80, 24)
	gone.release() // the departed shell's deferred release, arriving late

	resizeFrame(reg, sid, fresh.shellID, 190, 55)
	fresh.mustBe(t, 190, 55)
}
