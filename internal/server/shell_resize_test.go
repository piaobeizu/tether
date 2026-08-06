package server

import (
	"net/url"
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

// recordResize registers a shell-resize hook for sid and returns a pointer to
// the last size it observed. cols/rows are -1 until the hook runs.
func recordResize(reg *session.Registry, sid string) *[2]int {
	got := &[2]int{-1, -1}
	reg.RegisterShellResize(sid, func(cols, rows uint16) error {
		got[0], got[1] = int(cols), int(rows)
		return nil
	})
	return got
}

func TestRouteClientFrame_ResizeReachesTheShellHook(t *testing.T) {
	reg := session.NewRegistry()
	got := recordResize(reg, "sid-1")

	resp, ok := routeClientFrame(reg, wire.ClientFrame{
		Kind:      wire.ClientFrameResize,
		SessionID: "sid-1",
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
	target := recordResize(reg, "sid-target")
	other := recordResize(reg, "sid-other")

	routeClientFrame(reg, wire.ClientFrame{
		Kind: wire.ClientFrameResize, SessionID: "sid-target", Cols: 100, Rows: 30,
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
	got := recordResize(reg, "")

	routeClientFrame(reg, wire.ClientFrame{
		Kind: wire.ClientFrameResize, SessionID: "", Cols: 80, Rows: 24,
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
			got := recordResize(reg, "sid-1")
			routeClientFrame(reg, wire.ClientFrame{
				Kind: wire.ClientFrameResize, SessionID: "sid-1", Cols: tc.cols, Rows: tc.rows,
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

	detach := attachShellResize(reg, "sid-e2e", ptmx)

	routeClientFrame(reg, wire.ClientFrame{
		Kind: wire.ClientFrameResize, SessionID: "sid-e2e", Cols: 176, Rows: 48,
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
		Kind: wire.ClientFrameResize, SessionID: "sid-e2e", Cols: 100, Rows: 30,
	})
	ws, err = pty.GetsizeFull(ptmx)
	if err != nil {
		t.Fatalf("GetsizeFull after detach: %v", err)
	}
	if ws.Cols != 176 || ws.Rows != 48 {
		t.Fatalf("detached shell still resized to cols=%d rows=%d", ws.Cols, ws.Rows)
	}
}
