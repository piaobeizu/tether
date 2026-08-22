package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"sync"

	"github.com/creack/pty"
	"github.com/quic-go/webtransport-go"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/auth"
	"github.com/piaobeizu/tether/internal/permission/cchook"
	"github.com/piaobeizu/tether/internal/session"
)

// handleWTShell handles /wt/shell WebTransport upgrades (s6 / D-05a §2 fact 4).
// The connection carries raw PTY bytes — no JSON framing, no envelope wrapping.
// xterm.js on the browser side consumes the raw stream directly.
func handleWTShell(reg *session.Registry, wts *webtransport.Server, authState *auth.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if authState.ClientIDFromTicket(r) == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		sid := r.URL.Query().Get("sid")

		wtSess, err := wts.Upgrade(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		ctx := wtSess.Context()

		// Accept the bidi stream the browser opens.
		stream, err := wtSess.AcceptStream(ctx)
		if err != nil {
			_ = wtSess.CloseWithError(0, "")
			return
		}

		// Spawn claude under PTY. cc internally coordinates jsonl with any
		// concurrent chat subprocess (D-05a §2 fact 3).
		// WorkdirForSession, not reg.Workdir: since tether#52 the chat session this
		// shell is about to `--resume` may live in any registered workspace, and a
		// shell in a different directory finds no conversation there (see
		// buildPTYCommand). The sid is enough to look it up — the shell needs no
		// workspace parameter of its own, and giving it one would let the two
		// disagree.
		cmd := buildPTYCommand(ctx, ResolveClaudePath(), sid, reg.WorkdirForSession(sid))
		cmd.Env = buildPTYEnv(reg.PermGate)

		// Start the PTY at the size the browser already knows it will render
		// at, rather than at the kernel default. Waiting for the first resize
		// frame instead would paint one screenful of TUI at the wrong width and
		// then reflow it, and would leave the size wrong for the whole session
		// whenever /wt/control never connects.
		ptmx, err := startPTY(cmd, parseWinsize(r.URL.Query()))
		if err != nil {
			_, _ = stream.Write([]byte("\r\n[tether] failed to start shell: " + err.Error() + "\r\n"))
			_ = stream.Close()
			_ = wtSess.CloseWithError(1, "pty start failed")
			return
		}

		defer attachShellResize(reg, sid, ptmx)()

		pumpShell(ctx, stream, ptmx, cmd.Wait, func() { _ = wtSess.CloseWithError(0, "") })
	}
}

// pumpShell runs the two byte pumps of one /wt/shell connection and owns the
// teardown of all three things it is handed: the PTY master, the stream, and the
// WebTransport session.
//
// Extracted from handleWTShell rather than inlined there for the reason
// attachShellResize gives above — the handler takes a concrete
// *webtransport.Session, so nothing in it is reachable without a real QUIC
// connection, and this teardown is where tether#121's leak lived. Everything the
// fix consists of is in here, behind interfaces a test can supply.
//
// # A shell now ends for exactly two reasons
//
// The PTY process exited, or the client went away. There is no third arm and no
// clock: until tether#121 a 60-second timer in the session lock closed a
// `preempted` channel this select also watched, so a shell whose user was TYPING
// (input never touched the lock) was killed mid-keystroke and told "session taken
// over" by nobody. internal/session/lock.go went with it.
//
// # Why all three closes are unconditional
//
// The `<-done` arm — a user typing `exit`, the single most ordinary way a shell
// ends — used to have an empty body and fall straight through to a bare
// stream.Close(). That left, for as long as the browser held its QUIC connection:
//
//   - ptmx, the PTY master fd, never closed. ~1024 shell opens in one tab exhaust
//     the daemon's descriptor limit, and what breaks then is not the shell — it is
//     the next cert load and every listener with it.
//   - the WT→PTY pump, parked forever in io.Copy reading `stream`.
//     webtransport.Stream.Close() closes the SEND direction only (stream.go:404,
//     "It does not close the receive-direction"), so the stream close below cannot
//     wake it. Only destroying the session can.
//   - the session itself, still registered with the webtransport server.
//     Server.Upgrade builds it with context.WithoutCancel(r.Context())
//     (server.go:379) precisely so the handler returning does NOT end it, over a
//     CONNECT stream it hijacked from http3 (server.go:366) so http3 will not end
//     it either. Nobody else was ever going to.
//
// So closeSession is not optional, and /wt/shell was the only WebTransport route
// in this package without it — serveChat (wt_chat.go), serveControl (control.go)
// and serveEvents (wt_events.go) all open with `defer CloseWithError(0, "")`.
func pumpShell(ctx context.Context, stream io.ReadWriteCloser, ptmx io.ReadWriteCloser, wait func() error, closeSession func()) {
	// Deferred as well as called explicitly below. The defer is what makes "every
	// exit from here destroys the session" true of a panic too; the explicit call
	// is what puts it BEFORE wait(), so the WT→PTY pump is woken before this
	// goroutine blocks on the child rather than after. Calling it twice is free:
	// Session.CloseWithError returns at session.go:397 when it was not the first
	// caller.
	defer closeSession()

	done := make(chan struct{})
	var closeOnce sync.Once
	closePTY := func() { closeOnce.Do(func() { _ = ptmx.Close() }) }

	// PTY → WT: forward raw output bytes.
	go func() {
		defer close(done)
		_, _ = io.Copy(stream, ptmx)
	}()

	// WT → PTY: forward keyboard input.
	go func() {
		_, _ = io.Copy(ptmx, stream)
		closePTY()
	}()

	select {
	case <-done:
		// The PTY process exited on its own — `exit`, or cc quitting.
	case <-ctx.Done():
		// The WebTransport session went away: tab closed, network gone.
	}

	// One teardown for both arms. Closing the master is also what makes wait()
	// return rather than block: it hangs up the child's controlling terminal.
	closePTY()
	closeSession()
	_ = stream.Close()
	_ = wait()
}

// attachShellResize routes later /wt/control resize frames to this PTY and
// returns the detach func, so the caller can write `defer attach(...)()`.
//
// Size changes cannot ride the /wt/shell stream — it is raw PTY bytes by
// contract (D-05a §2 fact 4) — so they arrive on /wt/control and are routed
// back by sid. tether#68.
//
// Routing by sid alone was safe while the shell lock refused a second /wt/shell
// for a sid that already had one. tether#121 removed that lock, so two shells on
// one sid is now an ordinary state (two tabs, two devices) and the LAST one to
// register owns the sid's resize slot: the older pane's resizes reach the newer
// pane's PTY, and the newer pane's deferred detach drops the slot entirely. That
// is a real regression of tether#68, left standing here deliberately rather than
// papered over — fixing it means keying this map by shell instance instead of by
// sid, which is a change to Registry's shape and not to this file.
//
// Extracted from handleWTShell (rather than inlined there) so the whole path
// from a control frame to the kernel's winsize is reachable from a test: the
// handler itself needs a live WebTransport session, and an untested seam here
// is exactly the kind that keeps compiling while doing nothing.
func attachShellResize(reg *session.Registry, sid string, ptmx *os.File) func() {
	reg.RegisterShellResize(sid, func(cols, rows uint16) error {
		return pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols})
	})
	return func() { reg.UnregisterShellResize(sid) }
}

// parseWinsize reads the terminal size the browser reported on the /wt/shell
// query string. Returns nil when either dimension is absent, unparseable, or
// zero — the caller then starts the PTY at the kernel default, which is what
// every shell did before tether#68. A bad size is not worth failing a shell
// over; it degrades to the old behaviour and the first resize frame corrects it.
func parseWinsize(q url.Values) *pty.Winsize {
	cols, errCols := strconv.ParseUint(q.Get("cols"), 10, 16)
	rows, errRows := strconv.ParseUint(q.Get("rows"), 10, 16)
	if errCols != nil || errRows != nil || cols == 0 || rows == 0 {
		return nil
	}
	return &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}
}

// startPTY starts cmd on a PTY, sized when the client told us a size and at
// the kernel default when it did not.
func startPTY(cmd *exec.Cmd, ws *pty.Winsize) (*os.File, error) {
	if ws == nil {
		return pty.Start(cmd)
	}
	return pty.StartWithSize(cmd, ws)
}

// buildPTYCommand builds the `claude` invocation for the PTY shell pane.
//
// The working directory is load-bearing, not cosmetic: the shell pane is handed
// the *chat* session's sid (web/src/panes/shell/index.tsx reads it from the
// store) and resumes it with `--resume <sid>`, but cc stores conversations under
// ~/.claude/projects/<encoded-cwd>/ — so a shell whose cwd differs from the cwd
// the chat session was created in gets "No conversation found" and drops the
// user into a fresh conversation. Both spawn paths therefore resolve their cwd
// through the same agent.ResolveWorkdir (tether#51); "" means "inherit the
// daemon's cwd", which is the pre-tether#51 behaviour.
//
// Since tether#52 workdir is the directory of THAT SESSION's workspace — the
// caller gets it from session.Registry.WorkdirForSession(sid), which falls back
// to the daemon-global root for a sid it knows nothing about. It is no longer
// simply Registry.Workdir, because chat is no longer always there.
//
// Extracted as a plain function so this contract is unit-testable without
// standing up a WebTransport session and a PTY.
func buildPTYCommand(ctx context.Context, ccPath, sid, workdir string) *exec.Cmd {
	var args []string
	if sid != "" {
		args = append(args, "--resume", sid)
	}
	cmd := exec.CommandContext(ctx, ccPath, args...)
	cmd.Dir = agent.ResolveWorkdir(workdir)
	return cmd
}

// buildPTYEnv constructs the env for the PTY shell subprocess.
// IS_SANDBOX=1 injected for root (D-05a §2 fact 5). TERM set for full TUI.
//
// gate carries BOTH the permission callback endpoint and the
// TETHER_DAEMON_MANAGED mark, and it is appended whole rather than unpacked
// here. This is the shell half of tether#149: the chat half
// (session.Registry.spawnEntry) builds a different environment from the SAME
// cchook.Gate, and the entries the two produce must agree exactly. Until
// tether#149 this function took a bare endpoint string, so a mark added on the
// chat path would have left every PTY shell unmarked — and an unmarked child is
// one the hook lets through by design, since it reads "unmarked" as "not a cc
// the daemon spawned". Nothing inside this package could have noticed.
func buildPTYEnv(gate cchook.Gate) []string {
	env := os.Environ()
	env = append(env, "TERM=xterm-256color")
	if os.Geteuid() == 0 {
		env = append(env, "IS_SANDBOX=1")
	}
	// Appended last so these win: os.Environ() above may already carry a stale
	// TETHER_DAEMON_* from the daemon's own parent, and exec.Cmd keeps the LAST
	// value for a duplicated key.
	return append(env, gate.Env()...)
}
